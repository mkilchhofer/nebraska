package auth

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"strings"
	"sync"

	"github.com/gin-gonic/gin"
	"github.com/google/go-github/v28/github"
	"golang.org/x/oauth2"
	githuboauth "golang.org/x/oauth2/github"

	"github.com/kinvolk/nebraska/cmd/nebraska/ginhelpers"
	"github.com/kinvolk/nebraska/pkg/random"
	"github.com/kinvolk/nebraska/pkg/sessions"
	ginsessions "github.com/kinvolk/nebraska/pkg/sessions/gin"
	"github.com/kinvolk/nebraska/pkg/sessions/memcache"
	memcachegob "github.com/kinvolk/nebraska/pkg/sessions/memcache/gob"
	"github.com/kinvolk/nebraska/pkg/sessions/securecookie"
)

type (
	GithubAuthConfig struct {
		EnterpriseURL     string
		SessionAuthKey    []byte
		SessionCryptKey   []byte
		OAuthClientID     string
		OAuthClientSecret string
		WebhookSecret     string
		ReadWriteTeams    []string
		ReadOnlyTeams     []string
		DefaultTeamID     string
	}

	githubTeamData struct {
		org  string
		team *string
	}

	stringSet              map[string]struct{}
	teamToUsersMap         map[string]stringSet
	sessionIDToTeamDataMap map[string]githubTeamData
	userSessionMap         map[string]sessionIDToTeamDataMap

	githubAuth struct {
		enterpriseURL string

		webhookSecret string
		oauthConfig   *oauth2.Config

		sessionsStore *sessions.Store

		userInfoLock   sync.Mutex
		userSessionIDs userSessionMap
		teamToUsers    teamToUsersMap

		readWriteTeams []string
		readOnlyTeams  []string
		defaultTeamID  string
	}
)

var (
	_ Authenticator = &githubAuth{}
)

func NewGithubAuthenticator(config *GithubAuthConfig) Authenticator {
	endpoint := githuboauth.Endpoint
	if config.EnterpriseURL != "" {
		endpoint = oauth2.Endpoint{
			AuthURL:  config.EnterpriseURL + "/login/oauth/authorize",
			TokenURL: config.EnterpriseURL + "/login/oauth/access_token",
		}
	}

	return &githubAuth{
		enterpriseURL: config.EnterpriseURL,
		webhookSecret: config.WebhookSecret,
		oauthConfig: &oauth2.Config{
			ClientID:     config.OAuthClientID,
			ClientSecret: config.OAuthClientSecret,
			// We are using following APIs:
			//
			// https://developer.github.com/v3/teams/#list-user-teams
			//
			// https://developer.github.com/v3/orgs/#list-your-organizations
			//
			// https://developer.github.com/v3/users/#get-the-authenticated-user
			//
			// Common required scope in those APIs seems
			// to be "user". Listing teams and orgs can be
			// done also with "read:org" scope. We don't
			// need "user" scope really as all we need is
			// just login and that's public information
			// accessible without any scope at all.
			Scopes:   []string{"read:org"},
			Endpoint: endpoint,
		},

		sessionsStore:  newSessionsStore(config),
		userSessionIDs: make(userSessionMap),
		teamToUsers:    make(teamToUsersMap),
		readWriteTeams: copyStringSlice(config.ReadWriteTeams),
		readOnlyTeams:  copyStringSlice(config.ReadOnlyTeams),
		defaultTeamID:  config.DefaultTeamID,
	}
}

func newSessionsStore(config *GithubAuthConfig) *sessions.Store {
	cache := memcache.New(memcachegob.New())
	codec := securecookie.New(config.SessionAuthKey, config.SessionCryptKey)
	return sessions.NewStore(cache, codec)
}

func copyStringSlice(original []string) []string {
	dup := make([]string, len(original))
	copy(dup, original)
	return dup
}

func (gha *githubAuth) SetupRouter(router ginhelpers.Router) {
	router.Use(ginsessions.SessionsMiddleware(gha.sessionsStore, "githubauth"))
	oauthRouter := router.Group("/login", "oauth")
	oauthRouter.GET("/cb", gha.loginCb)
	oauthRouter.POST("/webhook", gha.loginWebhook)
}

func (gha *githubAuth) Authenticate(c *gin.Context) (teamID string, replied bool) {
	session := ginsessions.GetSession(c)
	if session.Has("teamID") {
		if session.Get("accesslevel") != "rw" {
			if c.Request.Method != "HEAD" && c.Request.Method != "GET" {
				httpError(c, http.StatusForbidden)
				teamID = ""
				replied = true
				return
			}
		}
		teamID = session.Get("teamID").(string)
		replied = false
		return
	}
	authHeader := c.Request.Header.Get("Authorization")
	if authHeader == "" {
		oauthState := random.String(64)
		session.Set("state", oauthState)
		session.Set("desiredurl", c.Request.URL.String())
		sessionSave(c, session, "authMissingTeamID")
		logger.Debug().Str("oauthstate", oauthState).Msg("authenticate")
		url := gha.oauthConfig.AuthCodeURL(oauthState, oauth2.AccessTypeOnline)
		logger.Debug().Str("redirecting to", url).Msg("authenticate")
		redirectTo(c, url)
		teamID = ""
		replied = true
	} else {
		failed := true
		defer func() {
			if failed {
				teamID = ""
				replied = true
				gha.cleanupSession(c)
				httpError(c, http.StatusUnauthorized)
			}
		}()
		splitToken := strings.Fields(authHeader)
		if len(splitToken) != 2 {
			logger.Debug().Str("malformed authorization header", authHeader).Msg("auth metrics")
			return
		}
		if strings.ToLower(strings.TrimSpace(splitToken[0])) != "bearer" {
			logger.Debug().Str("authorization is not a bearer token", authHeader).Msg("auth metrics")
			return
		}
		bearerToken := strings.TrimSpace(splitToken[1])
		logger.Debug().Str("going to do the login dance with token", bearerToken).Msg("auth metrics")
		token := oauth2.Token{
			AccessToken: bearerToken,
		}
		tokenSource := oauth2.StaticTokenSource(&token)
		oauthClient := oauth2.NewClient(c.Request.Context(), tokenSource)
		failed = false
		if replied = gha.doLoginDance(c, oauthClient); !replied {
			teamID = session.Get("teamID").(string)
		} else {
			teamID = ""
		}
	}
	return
}

func (gha *githubAuth) loginCb(c *gin.Context) {
	const (
		resultOK = iota
		resultUnauthorized
		resultInternalFailure
	)
	result := resultInternalFailure
	defer func() {
		switch result {
		case resultOK:
		case resultUnauthorized:
			gha.cleanupSession(c)
			httpError(c, http.StatusUnauthorized)
		case resultInternalFailure:
			gha.cleanupSession(c)
			httpError(c, http.StatusInternalServerError)
		}
	}()

	session := ginsessions.GetSession(c)
	defer sessionSave(c, session, "login cb")
	desiredURL, ok := session.Get("desiredurl").(string)
	if !ok {
		logger.Error().Str("login cb", "expected to have a valid desiredurl item in session data").Send()
		return
	}
	state := c.Request.FormValue("state")
	logger.Debug().Str("state", state).Msg("login cb received oauth")
	expectedState, ok := session.Get("state").(string)
	if !ok {
		logger.Error().Str("login cb", "expected to have a valid state item in session data").Send()
		return
	}

	if expectedState != state {
		logger.Error().Str("expected", expectedState).Str("got", state).Msg("login cb: invalid oauth state")
		return
	}
	code := c.Request.FormValue("code")
	logger.Debug().Str("code", code).Msg("login cb received")
	token, err := gha.oauthConfig.Exchange(c.Request.Context(), code)
	if err != nil {
		logger.Error().Err(err).Msg("login cb: oauth exchange failed error")
		return
	}
	logger.Debug().Msgf("login cb received token %s", token)
	if !token.Valid() {
		logger.Error().Err(fmt.Errorf("login cb got invalid token")).Send()
		return
	}

	oauthClient := gha.oauthConfig.Client(c.Request.Context(), token)
	result = resultOK
	if replied := gha.doLoginDance(c, oauthClient); !replied {
		redirectTo(c, desiredURL)
	}
}

func (gha *githubAuth) loginWebhook(c *gin.Context) {
	signature := c.Request.Header.Get("X-Hub-Signature")
	if len(signature) == 0 {
		logger.Debug().Str("webhook", "request with missing signature, ignoring it").Send()
		return
	}
	eventType := c.Request.Header.Get("X-Github-Event")
	rawPayload, err := ioutil.ReadAll(c.Request.Body)
	if err != nil {
		logger.Debug().Str("failed to read the contents of the message", eventType).Msg("webhook")
		return
	}
	mac := hmac.New(sha1.New, []byte(gha.webhookSecret))
	_, _ = mac.Write(rawPayload)
	payloadMAC := hex.EncodeToString(mac.Sum(nil))
	// [5:] is to drop the "sha1-" part.
	if !hmac.Equal([]byte(signature[5:]), []byte(payloadMAC)) {
		logger.Debug().Str("webhook", "message validation failed").Send()
		return
	}
	payloadReader := bytes.NewBuffer(rawPayload)
	logger.Debug().Str("got event of type", eventType).Msg("webhook")
	switch eventType {
	case "github_app_authorization":
		gha.loginWebhookAuthorizationEvent(c, payloadReader)
	case "organization":
		gha.loginWebhookOrganizationEvent(c, payloadReader)
	case "membership":
		gha.loginWebhookMembershipEvent(c, payloadReader)
	case "team":
		gha.loginWebhookTeamEvent(c, payloadReader)
	default:
		logger.Debug().Str("ignoring event", eventType).Msg("webhook")
		return
	}
}

func (gha *githubAuth) doLoginDance(c *gin.Context, oauthClient *http.Client) (replied bool) {
	const (
		resultOK = iota
		resultUnauthorized
		resultInternalFailure
	)

	result := resultUnauthorized
	session := ginsessions.GetSession(c)
	defer func() {
		replied = true
		switch result {
		case resultOK:
			replied = false
		case resultUnauthorized:
			gha.cleanupSession(c)
			httpError(c, http.StatusUnauthorized)
		case resultInternalFailure:
			httpError(c, http.StatusInternalServerError)
		default:
			httpError(c, http.StatusInternalServerError)
		}
	}()

	client := github.NewClient(oauthClient)
	if gha.enterpriseURL != "" {
		var err error
		client, err = github.NewEnterpriseClient(
			gha.enterpriseURL+"/api/v3",
			gha.enterpriseURL+"/api/v3/upload",
			oauthClient)
		if err != nil {
			logger.Error().Err(err).Msg("create enterprise client failed to create")
			result = resultInternalFailure
			return
		}
	}

	ghUser, _, err := client.Users.Get(c.Request.Context(), "")
	if err != nil {
		logger.Error().Err(err).Str("login dance", "failed to get authenticated user").Send()
		result = resultInternalFailure
		return
	}
	if ghUser.Login == nil {
		logger.Error().Err(fmt.Errorf("login dance authenticated as a user without a login, meh")).Send()
		return
	}

	rwTeams := gha.readWriteTeams
	roTeams := gha.readOnlyTeams
	teamData := githubTeamData{}
	teamID := ""
	listOpts := github.ListOptions{
		Page:    1,
		PerPage: 50,
	}
	isRO := false
	isRW := false

checkLoop:
	for {
		ghTeams, response, err := client.Teams.ListUserTeams(c.Request.Context(), &listOpts)
		if err != nil {
			logger.Error().Err(err).Str("login dance", "failed to get user teams").Send()
			result = resultInternalFailure
			return
		}
		for _, ghTeam := range ghTeams {
			if ghTeam.Name == nil {
				logger.Debug().Str("login dance", "unnamed github team").Send()
				continue
			}
			logger.Debug().Str("github team", *ghTeam.Name).Msg("login dance")
			if ghTeam.Organization == nil {
				logger.Debug().Str("login dance", "github team with no org").Send()
				continue
			}
			if ghTeam.Organization.Login == nil {
				logger.Debug().Str("login dance", "github team in unnamed organization")
				continue
			}
			logger.Debug().Str("github team in organization", *ghTeam.Organization.Login).Msg("login dance")
			fullGithubTeamName := makeTeamName(*ghTeam.Organization.Login, *ghTeam.Name)
			logger.Debug().Str("trying to find a matching ro or rw team", fullGithubTeamName).Msg("login dance")
			for _, roTeam := range roTeams {
				if isRO {
					break
				}
				if fullGithubTeamName == roTeam {
					logger.Debug().Str("found matching ro team", fullGithubTeamName).Msg("login dance")
					teamData.org = *ghTeam.Organization.Login
					teamData.team = ghTeam.Name
					teamID = gha.defaultTeamID
					isRO = true
					session.Set("accesslevel", "ro")
					break
				}
			}
			for _, rwTeam := range rwTeams {
				if fullGithubTeamName == rwTeam {
					logger.Debug().Str("found matching rw team", fullGithubTeamName).Msg("login dance")
					teamData.org = *ghTeam.Organization.Login
					teamData.team = ghTeam.Name
					teamID = gha.defaultTeamID
					isRW = true
					session.Set("accesslevel", "rw")
					break checkLoop
				}
			}
		}
		// Next page being zero means that we are on the last
		// page.
		if response.NextPage == 0 {
			break
		}
		listOpts.Page = response.NextPage
	}
	if !isRW {
		logger.Debug().Str("login dance", "no matching teams found, trying orgs").Send()
		listOpts.Page = 1
	checkLoop2:
		for {
			ghOrgs, response, err := client.Organizations.List(c.Request.Context(), "", &listOpts)
			if err != nil {
				logger.Error().Err(err).Str("login dance", "failed to get user orgs").Send()
				result = resultInternalFailure
				return
			}
			for _, ghOrg := range ghOrgs {
				if ghOrg.Login == nil {
					logger.Debug().Str("login dance", "unnamed github organization")
					continue
				}
				logger.Debug().Str("github org", *ghOrg.Login).Msg("login dance")
				logger.Debug().Str("trying to find a matching ro or rw team", *ghOrg.Login).Msg("login dance")
				nebraskaOrgName := *ghOrg.Login
				for _, roTeam := range roTeams {
					if isRO {
						break
					}
					if nebraskaOrgName == roTeam {
						logger.Debug().Str("found matching ro team", nebraskaOrgName).Msg("login dance")
						teamData.org = nebraskaOrgName
						teamID = gha.defaultTeamID
						isRO = true
						session.Set("accesslevel", "ro")
						break
					}
				}
				for _, rwTeam := range rwTeams {
					if nebraskaOrgName == rwTeam {
						logger.Debug().Str("found matching rw team", nebraskaOrgName).Msg("login dance")
						teamData.org = nebraskaOrgName
						teamID = gha.defaultTeamID
						session.Set("accesslevel", "rw")
						break checkLoop2
					}
				}
			}
			// Next page being zero means that we are on the last
			// page.
			if response.NextPage == 0 {
				break
			}
			listOpts.Page = response.NextPage
		}
	}
	if teamID == "" {
		logger.Debug().Str("login dance", "not authorized").Send()
		return
	}
	username := *ghUser.Login
	session.Set("teamID", teamID)
	session.Set("username", username)
	sessionSave(c, session, "login dance")
	gha.addSessionID(username, session.ID(), teamData)
	result = resultOK
	return
}

func (gha *githubAuth) addSessionID(username, sessionID string, teamData githubTeamData) {
	gha.userInfoLock.Lock()
	defer gha.userInfoLock.Unlock()
	sessionIDs := gha.userSessionIDs[username]
	if sessionIDs == nil {
		sessionIDs = make(sessionIDToTeamDataMap)
		gha.userSessionIDs[username] = sessionIDs
	}
	sessionIDs[sessionID] = teamData
	if teamData.team != nil {
		teamName := makeTeamName(teamData.org, *teamData.team)
		users := gha.teamToUsers[teamName]
		if users == nil {
			users = make(stringSet)
			gha.teamToUsers[teamName] = users
		}
		users[username] = struct{}{}
	}
}

func (gha *githubAuth) cleanupSession(c *gin.Context) {
	session := ginsessions.GetSession(c)
	defer session.Mark()
	username, ok := session.Get("username").(string)
	if !ok {
		return
	}
	sessionID := session.ID()
	gha.userInfoLock.Lock()
	defer gha.userInfoLock.Unlock()
	sessionIDs, ok := gha.userSessionIDs[username]
	if !ok {
		return
	}
	if teamData, ok := sessionIDs[sessionID]; ok {
		if teamData.team != nil {
			teamName := makeTeamName(teamData.org, *teamData.team)
			if usersSet, ok := gha.teamToUsers[teamName]; ok {
				delete(usersSet, username)
				if len(usersSet) == 0 {
					delete(gha.teamToUsers, teamName)
				}
			}
		}
		delete(sessionIDs, sessionID)
		if len(sessionIDs) == 0 {
			delete(gha.userSessionIDs, username)
		}
	}
}

func sessionSave(c *gin.Context, session *sessions.Session, msg string) {
	if err := ginsessions.SaveSession(c, session); err != nil {
		logger.Error().Err(err).Str("failed to save the session", msg).Send()
		httpError(c, http.StatusInternalServerError)
	}
}

func redirectTo(c *gin.Context, where string) {
	c.Redirect(http.StatusTemporaryRedirect, where)
}

type (
	ghUser struct {
		Login string `json:"login"`
	}

	ghAppAuthPayload struct {
		Action string `json:"action"`
		Sender ghUser `json:"sender"`
	}

	ghMembership struct {
		User ghUser `json:"user"`
	}

	ghOrganizationPayload struct {
		Action     string       `json:"action"`
		Membership ghMembership `json:"membership"`
		Org        ghUser       `json:"organization"`
	}

	ghTeam struct {
		Name string `json:"name"`
	}

	ghMembershipPayload struct {
		Action string `json:"action"`
		Scope  string `json:"scope"`
		Member ghUser `json:"member"`
		Team   ghTeam `json:"team"`
		Org    ghUser `json:"organization"`
	}

	ghChangesName struct {
		From string `json:"from"`
	}

	ghChanges struct {
		Name ghChangesName `json:"name"`
	}

	ghTeamPayload struct {
		Action  string    `json:"action"`
		Changes ghChanges `json:"changes"`
		Team    ghTeam    `json:"team"`
		Org     ghUser    `json:"organization"`
	}
)

func (gha *githubAuth) loginWebhookAuthorizationEvent(c *gin.Context, payloadReader io.Reader) {
	var payload ghAppAuthPayload
	if err := json.NewDecoder(payloadReader).Decode(&payload); err != nil {
		logger.Error().Err(err).Str("webhook", "error unmarshalling github_app_authorization payload").Send()
		httpError(c, http.StatusBadRequest)
		return
	}
	logger.Debug().Str("got github_app_authorization event with action", payload.Action).Msg("webhook")
	if payload.Action != "revoked" {
		logger.Debug().Str("ignoring github_app_authorization event with action", payload.Action).Msg("webhook")
		return
	}
	username := payload.Sender.Login
	logger.Debug().Str("dropping all the sessions of user", username).Msg("webhook")
	userSessionIDs := gha.stealUserSessionIDs(username)
	for _, sessionID := range userSessionIDs {
		logger.Debug().Str("dropping session", sessionID).Msg("webhook")
		gha.sessionsStore.MarkOrDestroySessionByID(sessionID)
	}
}

func (gha *githubAuth) stealUserSessionIDs(username string) []string {
	gha.userInfoLock.Lock()
	defer gha.userInfoLock.Unlock()
	var userSessionIDs []string
	if sessionIDsAndTeams, ok := gha.userSessionIDs[username]; ok {
		userSessionIDs = make([]string, 0, len(sessionIDsAndTeams))
		for sessionID, teamData := range sessionIDsAndTeams {
			userSessionIDs = append(userSessionIDs, sessionID)
			if teamData.team != nil {
				teamName := makeTeamName(teamData.org, *teamData.team)
				if usersSet, ok := gha.teamToUsers[teamName]; ok {
					delete(usersSet, username)
					if len(usersSet) == 0 {
						delete(gha.teamToUsers, teamName)
					}
				}
			}
		}
		delete(gha.userSessionIDs, username)
	}
	return userSessionIDs
}

func (gha *githubAuth) loginWebhookOrganizationEvent(c *gin.Context, payloadReader io.Reader) {
	var payload ghOrganizationPayload
	if err := json.NewDecoder(payloadReader).Decode(&payload); err != nil {
		logger.Error().Err(err).Str("webhook", "error unmarshalling organization payload").Send()
		httpError(c, http.StatusBadRequest)
		return
	}
	logger.Debug().Msgf("webhook got organization event with action %s", payload.Action)
	if payload.Action != "member_removed" {
		logger.Debug().Str("ignoring organization event with action", payload.Action).Msg("webhook")
		return
	}
	username := payload.Membership.User.Login
	org := payload.Org.Login
	sessionIDs := gha.stealUserSessionIDsForOrg(username, org)
	for _, sessionID := range sessionIDs {
		logger.Debug().Str("webhook dropping session", sessionID).Send()
		gha.sessionsStore.MarkOrDestroySessionByID(sessionID)
	}
}

func (gha *githubAuth) stealUserSessionIDsForOrg(username, org string) []string {
	gha.userInfoLock.Lock()
	defer gha.userInfoLock.Unlock()
	var userSessionIDs []string
	if sessionIDsAndTeams, ok := gha.userSessionIDs[username]; ok {
		var toDrop []string
		for sessionID, teamData := range sessionIDsAndTeams {
			if teamData.org == org && teamData.team == nil {
				userSessionIDs = append(userSessionIDs, sessionID)
				toDrop = append(toDrop, sessionID)
			}
		}
		for _, sessionID := range toDrop {
			delete(sessionIDsAndTeams, sessionID)
		}
		if len(sessionIDsAndTeams) == 0 {
			logger.Debug().Str("dropped all the sessions of user", username).Msg("webhook")
			delete(gha.userSessionIDs, username)
		}
	}
	return userSessionIDs
}

func (gha *githubAuth) loginWebhookMembershipEvent(c *gin.Context, payloadReader io.Reader) {
	var payload ghMembershipPayload
	if err := json.NewDecoder(payloadReader).Decode(&payload); err != nil {
		logger.Error().Err(err).Str("webhook", "error unmarshalling membership payload")
		httpError(c, http.StatusBadRequest)
		return
	}
	logger.Debug().Str("got membership event with action", payload.Action).Msg("webhook")

	logger.Debug().Str("got membership event with scope", payload.Scope).Msg("webhook")
	if payload.Scope != "team" {
		logger.Debug().Str("ignoring membership event with scope", payload.Scope).Msg("webhook")
		return
	}
	username := payload.Member.Login
	org := payload.Org.Login
	team := payload.Team.Name

	if payload.Action == "added" {
		for _, rwTeam := range gha.readWriteTeams {
			teamName := makeTeamName(org, team)
			if rwTeam == teamName {
				logger.Debug().Str("action", payload.Action).Str("dropping all the sessions of user", username).Msg("webhook")
				sessionIDs := gha.stealUserSessionIDs(username)
				for _, sessionID := range sessionIDs {
					logger.Debug().Str("action", payload.Action).Str("dropping session", sessionID).Str("user", username).Msg("webhook")
					gha.sessionsStore.MarkOrDestroySessionByID(sessionID)
				}
				break
			}
		}
	} else if payload.Action == "removed" {
		sessionIDs := gha.stealUserSessionIDsForOrgAndTeam(username, org, team)
		for _, sessionID := range sessionIDs {
			logger.Debug().Str("action", payload.Action).Str("dropping session", sessionID).Str("user", username).Msg("webhook")
			gha.sessionsStore.MarkOrDestroySessionByID(sessionID)
		}
	} else {
		logger.Debug().Str("ignoring membership event with action", payload.Action).Msg("webhook")
		return
	}
}

func (gha *githubAuth) stealUserSessionIDsForOrgAndTeam(username, org, team string) []string {
	gha.userInfoLock.Lock()
	defer gha.userInfoLock.Unlock()
	var userSessionIDs []string
	if sessionIDsAndTeams, ok := gha.userSessionIDs[username]; ok {
		var toDrop []string
		for sessionID, teamData := range sessionIDsAndTeams {
			if teamData.org == org && teamData.team != nil && *teamData.team == team {
				userSessionIDs = append(userSessionIDs, sessionID)
				toDrop = append(toDrop, sessionID)
				teamName := makeTeamName(teamData.org, *teamData.team)
				if usersSet, ok := gha.teamToUsers[teamName]; ok {
					delete(usersSet, username)
					if len(usersSet) == 0 {
						delete(gha.teamToUsers, teamName)
					}
				}
			}
		}
		for _, sessionID := range toDrop {
			delete(sessionIDsAndTeams, sessionID)
		}
		if len(sessionIDsAndTeams) == 0 {
			logger.Debug().Str("dropped all the sessions of user", username).Msg("webhook")
			delete(gha.userSessionIDs, username)
		}
	}
	return userSessionIDs
}

func (gha *githubAuth) loginWebhookTeamEvent(c *gin.Context, payloadReader io.Reader) {
	var payload ghTeamPayload
	if err := json.NewDecoder(payloadReader).Decode(&payload); err != nil {
		logger.Error().Err(err).Str("webhook", "error unmarshalling team payload").Send()
		httpError(c, http.StatusBadRequest)
		return
	}
	logger.Debug().Str("got team event with action", payload.Action).Msg("webhook")
	org := payload.Org.Login
	team := ""
	switch payload.Action {
	case "deleted":
		team = payload.Team.Name
	case "edited":
		if payload.Changes.Name.From == "" {
			logger.Debug().Msg("ignoring edited team event that does not rename the team")
			return
		}
		team = payload.Changes.Name.From
	default:
		logger.Debug().Str("ignoring team event with action", payload.Action).Msg("webhook")
		return
	}
	sessionIDs := gha.stealSessionIDsForOrgAndTeam(org, team)
	for _, sessionID := range sessionIDs {
		logger.Debug().Str("webhook dropping session", sessionID).Send()
		gha.sessionsStore.MarkOrDestroySessionByID(sessionID)
	}
}

func (gha *githubAuth) stealSessionIDsForOrgAndTeam(org, team string) []string {
	gha.userInfoLock.Lock()
	defer gha.userInfoLock.Unlock()
	var userSessionIDs []string
	teamName := makeTeamName(org, team)
	for username := range gha.teamToUsers[teamName] {
		if sessionIDsAndTeams, ok := gha.userSessionIDs[username]; ok {
			var toDrop []string
			for sessionID, teamData := range sessionIDsAndTeams {
				if teamData.org == org && teamData.team != nil && *teamData.team == team {
					userSessionIDs = append(userSessionIDs, sessionID)
					toDrop = append(toDrop, sessionID)
				}
			}
			for _, sessionID := range toDrop {
				delete(sessionIDsAndTeams, sessionID)
			}
			if len(sessionIDsAndTeams) == 0 {
				logger.Debug().Str("dropped all the sessions of user", username).Msg("webhook")
				delete(gha.userSessionIDs, username)
			}
		}
	}
	delete(gha.teamToUsers, teamName)
	return userSessionIDs
}

func httpError(c *gin.Context, status int) {
	c.AbortWithStatus(status)
}

func makeTeamName(org, team string) string {
	return fmt.Sprintf("%s/%s", org, team)
}
