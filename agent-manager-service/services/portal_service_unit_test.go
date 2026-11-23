// Copyright (c) 2026, WSO2 LLC. (https://www.wso2.com).
//
// WSO2 LLC. licenses this file to you under the Apache License,
// Version 2.0 (the "License"); you may not use this file except
// in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing,
// software distributed under the License is distributed on an
// "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
// KIND, either express or implied.  See the License for the
// specific language governing permissions and limitations
// under the License.

package services

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wso2/agent-manager/agent-manager-service/clients/clientmocks"
	"github.com/wso2/agent-manager/agent-manager-service/clients/openchoreosvc/client"
	"github.com/wso2/agent-manager/agent-manager-service/models"
	"github.com/wso2/agent-manager/agent-manager-service/repositories"
	"github.com/wso2/agent-manager/agent-manager-service/repositories/repomocks"
	"github.com/wso2/agent-manager/agent-manager-service/utils"
)

const portalUserSub = "staff-7"

// portalChatKeyRecorder stands in for the API key service. A nil IssuePortalChatKeyFunc fails
// the test: the path must not mint a key.
type portalChatKeyRecorder struct {
	t                      *testing.T
	IssuePortalChatKeyFunc func(agentName, userSub, publicationID string) (*models.IssueTestAPIKeyResponse, error)
}

func (k *portalChatKeyRecorder) IssuePortalChatKey(
	_ context.Context, _, _, agentName, _, userSub, publicationID string,
) (*models.IssueTestAPIKeyResponse, error) {
	if k.IssuePortalChatKeyFunc == nil {
		k.t.Fatalf("no chat key may be issued on this path (agent %s)", agentName)
	}
	return k.IssuePortalChatKeyFunc(agentName, userSub, publicationID)
}

type endpointsFunc func(agentName string) (map[string]models.EndpointsResponse, error)

func (f endpointsFunc) GetAgentEndpoints(_ context.Context, _, _, agentName, _ string) (map[string]models.EndpointsResponse, error) {
	return f(agentName)
}

// portalFixture holds published agents, all available unless a test says otherwise: deployed,
// API-key security on, CORS allowing the portal.
type portalFixture struct {
	t          *testing.T
	pubs       []models.AgentPublication
	suspended  map[string]bool
	configs    map[string]*models.AgentConfig
	lookupErr  map[string]error
	endpoints  map[string]map[string]models.EndpointsResponse
	oc         *clientmocks.OpenChoreoClientMock
	repo       *repomocks.AgentPublicationRepositoryMock
	keys       *portalChatKeyRecorder
	listFilter models.AgentPublicationFilter
}

func newPortalFixture(t *testing.T, agentNames ...string) *portalFixture {
	t.Helper()
	f := &portalFixture{
		t:         t,
		suspended: map[string]bool{},
		configs:   map[string]*models.AgentConfig{},
		lookupErr: map[string]error{},
		endpoints: map[string]map[string]models.EndpointsResponse{},
		keys:      &portalChatKeyRecorder{t: t},
	}
	for _, name := range agentNames {
		f.pubs = append(f.pubs, models.AgentPublication{
			ID: uuid.New(), OUID: pubOUID, ProjectName: pubProject, AgentName: name,
			EnvironmentUUID: pubEnvUUID, EnvironmentName: pubEnv, DisplayName: name, Tags: []string{"HR"},
		})
		f.configs[name] = &models.AgentConfig{
			EnableApiKeySecurity: true, CORSEnabled: true, CORSAllowOrigins: []string{pubOwnerOrigin, pubPortalOrigin},
		}
		f.endpoints[name] = map[string]models.EndpointsResponse{
			"default": {Endpoint: models.Endpoint{URL: "https://gw.example.com/" + name}},
		}
	}
	f.oc = &clientmocks.OpenChoreoClientMock{
		GetReleaseBindingRolloutFunc: func(_ context.Context, _, agentName, _ string) (client.ReleaseBindingRollout, error) {
			if err := f.lookupErr[agentName]; err != nil {
				return client.ReleaseBindingRollout{}, err
			}
			return client.ReleaseBindingRollout{Deployed: !f.suspended[agentName]}, nil
		},
	}
	f.repo = &repomocks.AgentPublicationRepositoryMock{
		ListFunc: func(_ context.Context, ouID string, filter models.AgentPublicationFilter) ([]models.AgentPublication, int64, error) {
			assert.Equal(t, pubOUID, ouID)
			f.listFilter = filter
			return f.pubs, int64(len(f.pubs)), nil
		},
		ListTagsFunc: func(_ context.Context, _ string) ([]string, error) { return []string{"HR", "IT"}, nil },
		GetByIDFunc: func(_ context.Context, ouID string, id uuid.UUID) (*models.AgentPublication, error) {
			for i := range f.pubs {
				if f.pubs[i].ID == id && f.pubs[i].OUID == ouID {
					return &f.pubs[i], nil
				}
			}
			return nil, repositories.ErrAgentPublicationNotFound
		},
	}
	return f
}

func (f *portalFixture) svcWithOrigin(origin string) PortalService {
	configs := &repomocks.AgentConfigRepositoryMock{
		GetFunc: func(_ context.Context, _, _, agentName, _ string) (*models.AgentConfig, error) {
			if cfg := f.configs[agentName]; cfg != nil {
				return cfg, nil
			}
			return nil, repositories.ErrAgentConfigNotFound
		},
	}
	endpoints := endpointsFunc(func(agentName string) (map[string]models.EndpointsResponse, error) {
		return f.endpoints[agentName], nil
	})
	return NewPortalService(f.repo, configs, f.oc, endpoints, f.keys, origin, discardLogger())
}

func (f *portalFixture) svc() PortalService { return f.svcWithOrigin(pubPortalOrigin) }

func (f *portalFixture) allowKeys() {
	f.keys.IssuePortalChatKeyFunc = func(_, _, _ string) (*models.IssueTestAPIKeyResponse, error) {
		connected := true
		return &models.IssueTestAPIKeyResponse{APIKey: "key-value", ExpiresAt: "2026-11-23T10:42:00Z", GatewayConnected: &connected}, nil
	}
}

func availability(page *models.PortalAgentPage) map[string]bool {
	out := map[string]bool{}
	for _, a := range page.Agents {
		out[a.Publication.AgentName] = a.Available
	}
	return out
}

// ---- ListAgents ----

func TestPortalListMarksEachAgentsAvailability(t *testing.T) {
	f := newPortalFixture(t, "ok", "suspended", "cors-drift", "oauth", "cors-off", "no-settings")
	f.suspended["suspended"] = true
	f.configs["cors-drift"].CORSAllowOrigins = []string{pubOwnerOrigin}
	f.configs["oauth"].EnableApiKeySecurity, f.configs["oauth"].EnableOAuthSecurity = false, true
	f.configs["cors-off"].CORSEnabled = false
	f.configs["no-settings"] = nil // the platform defaults: API key on, CORS on, origin "*"

	page, err := f.svc().ListAgents(context.Background(), pubOUID, models.AgentPublicationFilter{})
	require.NoError(t, err)
	assert.Equal(t, map[string]bool{
		"ok": true, "suspended": false, "cors-drift": false, "oauth": false, "cors-off": false, "no-settings": true,
	}, availability(page))
	assert.Equal(t, []string{"HR", "IT"}, page.Tags)
	assert.EqualValues(t, 6, page.Total)
}

func TestPortalListShowsAnAgentWhoseLookupFailedAsUnavailable(t *testing.T) {
	f := newPortalFixture(t, "ok", "flaky")
	f.lookupErr["flaky"] = errors.New("openchoreo timeout")

	page, err := f.svc().ListAgents(context.Background(), pubOUID, models.AgentPublicationFilter{})
	require.NoError(t, err, "one failed lookup does not fail the catalog")
	assert.Equal(t, map[string]bool{"ok": true, "flaky": false}, availability(page))
}

func TestPortalListAppliesPagingDefaultsAndLimits(t *testing.T) {
	for name, tc := range map[string]struct {
		in, want models.AgentPublicationFilter
	}{
		"defaults":            {models.AgentPublicationFilter{}, models.AgentPublicationFilter{Limit: 20}},
		"limit capped at 100": {models.AgentPublicationFilter{Limit: 500}, models.AgentPublicationFilter{Limit: 100}},
		"negative offset":     {models.AgentPublicationFilter{Limit: 5, Offset: -3}, models.AgentPublicationFilter{Limit: 5}},
		"search and tag trimmed": {
			models.AgentPublicationFilter{Search: " leave ", Tag: " HR ", Limit: 10, Offset: 20},
			models.AgentPublicationFilter{Search: "leave", Tag: "HR", Limit: 10, Offset: 20},
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := newPortalFixture(t)
			page, err := f.svc().ListAgents(context.Background(), pubOUID, tc.in)
			require.NoError(t, err)
			assert.Equal(t, tc.want, f.listFilter)
			assert.Equal(t, tc.want.Limit, page.Limit)
			assert.Equal(t, tc.want.Offset, page.Offset)
		})
	}
}

func TestPortalListWithoutAConfiguredPortalListsEveryAgentUnavailable(t *testing.T) {
	f := newPortalFixture(t, "ok")
	f.oc.GetReleaseBindingRolloutFunc = nil // nothing to look up when no portal can reach the agent

	page, err := f.svcWithOrigin("").ListAgents(context.Background(), pubOUID, models.AgentPublicationFilter{})
	require.NoError(t, err)
	assert.Equal(t, map[string]bool{"ok": false}, availability(page))
}

func TestPortalListDoesNotMaskARepositoryError(t *testing.T) {
	f := newPortalFixture(t)
	boom := errors.New("database down")
	f.repo.ListFunc = func(_ context.Context, _ string, _ models.AgentPublicationFilter) ([]models.AgentPublication, int64, error) {
		return nil, 0, boom
	}

	_, err := f.svc().ListAgents(context.Background(), pubOUID, models.AgentPublicationFilter{})
	assert.ErrorIs(t, err, boom)
}

// ---- GetAgent ----

func TestPortalGetAgent(t *testing.T) {
	t.Run("returns the agent and its availability", func(t *testing.T) {
		f := newPortalFixture(t, "ok")
		agent, err := f.svc().GetAgent(context.Background(), pubOUID, f.pubs[0].ID)
		require.NoError(t, err)
		assert.Equal(t, "ok", agent.Publication.AgentName)
		assert.True(t, agent.Available)
	})
	t.Run("another organization's agent is not found", func(t *testing.T) {
		f := newPortalFixture(t, "ok")
		_, err := f.svc().GetAgent(context.Background(), "ou-other", f.pubs[0].ID)
		assert.ErrorIs(t, err, utils.ErrPortalAgentNotFound)
	})
	t.Run("a repository error is not reported as not found", func(t *testing.T) {
		f := newPortalFixture(t, "ok")
		boom := errors.New("database down")
		f.repo.GetByIDFunc = func(_ context.Context, _ string, _ uuid.UUID) (*models.AgentPublication, error) {
			return nil, boom
		}
		_, err := f.svc().GetAgent(context.Background(), pubOUID, f.pubs[0].ID)
		assert.ErrorIs(t, err, boom)
		assert.NotErrorIs(t, err, utils.ErrPortalAgentNotFound)
	})
}

// ---- IssueChatKey ----

func TestPortalChatKeyIssuesAKeyAndTheChatURL(t *testing.T) {
	f := newPortalFixture(t, "hr-helper")
	f.endpoints["hr-helper"] = map[string]models.EndpointsResponse{
		"zeta":    {Endpoint: models.Endpoint{URL: "https://gw.example.com/zeta"}},
		"default": {Endpoint: models.Endpoint{URL: "https://gw.example.com/people/hr-helper/"}},
	}
	var gotSub, gotPub string
	f.keys.IssuePortalChatKeyFunc = func(_, userSub, publicationID string) (*models.IssueTestAPIKeyResponse, error) {
		gotSub, gotPub = userSub, publicationID
		return &models.IssueTestAPIKeyResponse{APIKey: "key-value", ExpiresAt: "2026-11-23T10:42:00Z"}, nil
	}

	key, err := f.svc().IssueChatKey(publisherCtx(t), pubOUID, f.pubs[0].ID, portalUserSub)
	require.NoError(t, err)
	assert.Equal(t, "https://gw.example.com/people/hr-helper/chat", key.ChatURL,
		"the first endpoint by name, as Try-It picks it, with no double slash")
	assert.Equal(t, "key-value", key.APIKey)
	assert.False(t, key.GatewayConnected, "unknown connectivity is reported as not connected")
	assert.Equal(t, portalUserSub, gotSub, "one key per user")
	assert.Equal(t, f.pubs[0].ID.String(), gotPub)
}

func TestPortalChatKeyRefusesAnUnavailableAgentWithoutMintingAKey(t *testing.T) {
	for name, mutate := range map[string]func(f *portalFixture){
		"suspended":     func(f *portalFixture) { f.suspended["hr-helper"] = true },
		"CORS drift":    func(f *portalFixture) { f.configs["hr-helper"].CORSAllowOrigins = []string{pubOwnerOrigin} },
		"API key off":   func(f *portalFixture) { f.configs["hr-helper"].EnableApiKeySecurity = false },
		"no endpoint":   func(f *portalFixture) { f.endpoints["hr-helper"] = nil },
		"empty address": func(f *portalFixture) { f.endpoints["hr-helper"]["default"] = models.EndpointsResponse{} },
	} {
		t.Run(name, func(t *testing.T) {
			f := newPortalFixture(t, "hr-helper") // keys.IssuePortalChatKeyFunc stays nil
			mutate(f)

			_, err := f.svc().IssueChatKey(publisherCtx(t), pubOUID, f.pubs[0].ID, portalUserSub)
			assert.ErrorIs(t, err, utils.ErrPortalAgentUnavailable)
		})
	}
}

func TestPortalChatKeyReportsAFailedLookupAsAnError(t *testing.T) {
	f := newPortalFixture(t, "hr-helper")
	boom := errors.New("openchoreo timeout")
	f.lookupErr["hr-helper"] = boom

	_, err := f.svc().IssueChatKey(publisherCtx(t), pubOUID, f.pubs[0].ID, portalUserSub)
	assert.ErrorIs(t, err, boom, "the caller is told, not guessed for")
	assert.NotErrorIs(t, err, utils.ErrPortalAgentUnavailable)
}

func TestPortalChatKeyErrors(t *testing.T) {
	t.Run("an unknown agent is not found", func(t *testing.T) {
		f := newPortalFixture(t, "hr-helper")
		_, err := f.svc().IssueChatKey(publisherCtx(t), pubOUID, uuid.New(), portalUserSub)
		assert.ErrorIs(t, err, utils.ErrPortalAgentNotFound)
	})
	t.Run("a caller with no user is refused before any lookup", func(t *testing.T) {
		f := newPortalFixture(t, "hr-helper")
		f.repo.GetByIDFunc = nil
		_, err := f.svc().IssueChatKey(publisherCtx(t), pubOUID, f.pubs[0].ID, "")
		assert.ErrorIs(t, err, utils.ErrUnauthorized)
	})
	t.Run("no gateway for the environment is passed through", func(t *testing.T) {
		f := newPortalFixture(t, "hr-helper")
		f.keys.IssuePortalChatKeyFunc = func(_, _, _ string) (*models.IssueTestAPIKeyResponse, error) {
			return nil, utils.ErrGatewayNotFound
		}
		_, err := f.svc().IssueChatKey(publisherCtx(t), pubOUID, f.pubs[0].ID, portalUserSub)
		assert.ErrorIs(t, err, utils.ErrGatewayNotFound)
	})
	t.Run("a connected gateway is reported", func(t *testing.T) {
		f := newPortalFixture(t, "hr-helper")
		f.allowKeys()
		key, err := f.svc().IssueChatKey(publisherCtx(t), pubOUID, f.pubs[0].ID, portalUserSub)
		require.NoError(t, err)
		assert.True(t, key.GatewayConnected)
	})
}
