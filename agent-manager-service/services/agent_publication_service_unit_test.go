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

	"github.com/wso2/agent-manager/agent-manager-service/audit"
	"github.com/wso2/agent-manager/agent-manager-service/clients/clientmocks"
	"github.com/wso2/agent-manager/agent-manager-service/clients/openchoreosvc/client"
	"github.com/wso2/agent-manager/agent-manager-service/middleware/jwtassertion"
	"github.com/wso2/agent-manager/agent-manager-service/models"
	"github.com/wso2/agent-manager/agent-manager-service/rbac"
	"github.com/wso2/agent-manager/agent-manager-service/repositories"
	"github.com/wso2/agent-manager/agent-manager-service/repositories/repomocks"
	"github.com/wso2/agent-manager/agent-manager-service/spec"
	"github.com/wso2/agent-manager/agent-manager-service/utils"
)

const (
	pubOUID         = "ou-portal"
	pubProject      = "people"
	pubAgent        = "hr-helper"
	pubEnv          = "production"
	pubPortalOrigin = "https://portal.example.com"
	pubOwnerOrigin  = "http://localhost:3000"
	pubPublisherSub = "user-1"
)

var pubEnvUUID = uuid.MustParse("6f1c2c64-6a51-4f0e-9f53-2f2b5c0f6a11")

// deploySettingsRecorder stands in for the agent service's deploy-settings save. Like the real
// one, a CORS change is persisted, so a later read of the agent's config sees it. A nil
// UpdateAgentDeploySettingsFunc fails the test: the path must not change the agent's CORS.
type deploySettingsRecorder struct {
	t                             *testing.T
	config                        **models.AgentConfig
	UpdateAgentDeploySettingsFunc func(req *spec.UpdateAgentDeploySettingsRequest) error
	origins                       [][]string
}

func (d *deploySettingsRecorder) UpdateAgentDeploySettings(
	_ context.Context, _, _, _ string, req *spec.UpdateAgentDeploySettingsRequest,
) error {
	if d.UpdateAgentDeploySettingsFunc == nil {
		d.t.Fatalf("the agent's CORS must not change on this path, but it was set to %v", req.CorsConfig.AllowOrigin)
	}
	d.origins = append(d.origins, req.CorsConfig.AllowOrigin)
	if err := d.UpdateAgentDeploySettingsFunc(req); err != nil {
		return err
	}
	(*d.config).CORSAllowOrigins = req.CorsConfig.AllowOrigin
	return nil
}

// publicationFixture is an eligible chat-api agent deployed in a production environment, not yet
// published. Tests change the fields before calling svc().
type publicationFixture struct {
	t        *testing.T
	agent    *models.AgentResponse
	env      *models.EnvironmentResponse
	config   *models.AgentConfig
	deployed bool
	oc       *clientmocks.OpenChoreoClientMock
	configs  *repomocks.AgentConfigRepositoryMock
	repo     *repomocks.AgentPublicationRepositoryMock
	deploy   *deploySettingsRecorder
	upserted []*models.AgentPublication
}

func newPublicationFixture(t *testing.T) *publicationFixture {
	t.Helper()
	f := &publicationFixture{
		t: t,
		agent: &models.AgentResponse{
			Name:         pubAgent,
			ProjectName:  pubProject, // getAgent treats an agent in another project as missing
			Provisioning: models.Provisioning{Type: string(utils.InternalAgent)},
			Type:         models.AgentType{Type: string(utils.AgentTypeAPI), SubType: string(utils.AgentSubTypeChatAPI)},
		},
		env:      &models.EnvironmentResponse{UUID: pubEnvUUID.String(), Name: pubEnv, IsProduction: true},
		config:   &models.AgentConfig{EnableApiKeySecurity: true, CORSEnabled: true, CORSAllowOrigins: []string{pubOwnerOrigin}},
		deployed: true,
	}
	f.oc = &clientmocks.OpenChoreoClientMock{
		GetComponentFunc: func(_ context.Context, _, _, _ string) (*models.AgentResponse, error) {
			return f.agent, nil
		},
		GetEnvironmentFunc: func(_ context.Context, _, _ string) (*models.EnvironmentResponse, error) {
			return f.env, nil
		},
		GetReleaseBindingRolloutFunc: func(_ context.Context, _, _, _ string) (client.ReleaseBindingRollout, error) {
			return client.ReleaseBindingRollout{Deployed: f.deployed}, nil
		},
	}
	f.configs = &repomocks.AgentConfigRepositoryMock{
		GetFunc: func(_ context.Context, _, _, _, _ string) (*models.AgentConfig, error) {
			if f.config == nil {
				return nil, repositories.ErrAgentConfigNotFound
			}
			return f.config, nil
		},
	}
	f.repo = &repomocks.AgentPublicationRepositoryMock{
		GetByAgentFunc: func(_ context.Context, _, _, _ string) (*models.AgentPublication, error) {
			return nil, repositories.ErrAgentPublicationNotFound
		},
		UpsertFunc: func(_ context.Context, p *models.AgentPublication) (*models.AgentPublication, error) {
			f.upserted = append(f.upserted, p)
			saved := *p
			saved.ID = uuid.New()
			return &saved, nil
		},
	}
	f.deploy = &deploySettingsRecorder{t: t, config: &f.config}
	return f
}

func (f *publicationFixture) svc() AgentPublicationService {
	return NewAgentPublicationService(f.repo, f.configs, f.oc, f.deploy, pubPortalOrigin, discardLogger())
}

// allowCORSChanges lets the path change the agent's CORS, as the real deploy-settings save would.
func (f *publicationFixture) allowCORSChanges() {
	f.deploy.UpdateAgentDeploySettingsFunc = func(*spec.UpdateAgentDeploySettingsRequest) error { return nil }
}

// publisherCtx is an owner allowed to publish: both environment tiers, as the production check needs.
func publisherCtx(t *testing.T) context.Context {
	t.Helper()
	return jwtassertion.ContextWithTokenClaimsAndScope(auditableCtx(t), &jwtassertion.TokenClaims{
		Sub:   pubPublisherSub,
		OuId:  pubOUID,
		Scope: audit.ScopesOf([]rbac.Permission{rbac.AgentEnvNonProduction, rbac.AgentEnvProduction}),
	})
}

func validListing() models.AgentPublicationRequest {
	return models.AgentPublicationRequest{DisplayName: "HR Helper", Description: "Leave and benefits", Tags: []string{"HR"}}
}

func checkByID(t *testing.T, checks []models.AgentPublicationCheck, id string) models.AgentPublicationCheck {
	t.Helper()
	for _, c := range checks {
		if c.ID == id {
			return c
		}
	}
	t.Fatalf("no %s check in %+v", id, checks)
	return models.AgentPublicationCheck{}
}

// ---- GetPublication ----

func TestAgentPublicationGetReportsEveryCheckAndThisEnvironmentsPublication(t *testing.T) {
	f := newPublicationFixture(t)
	published := &models.AgentPublication{ID: uuid.New(), EnvironmentUUID: pubEnvUUID, DisplayName: "HR Helper"}
	f.repo.GetByAgentFunc = func(_ context.Context, _, _, _ string) (*models.AgentPublication, error) {
		return published, nil
	}

	// A plain context: reading needs no environment-tier permission.
	status, err := f.svc().GetPublication(context.Background(), pubOUID, pubProject, pubAgent, pubEnv)
	require.NoError(t, err)
	assert.Same(t, published, status.Publication)
	assert.True(t, status.Eligible())
	ids := make([]string, 0, len(status.Checks))
	for _, c := range status.Checks {
		ids = append(ids, c.ID)
		assert.Empty(t, c.Reason, "a passed check has no reason")
	}
	assert.Equal(t, []string{
		models.PublicationCheckProductionEnvironment, models.PublicationCheckChatAPI, models.PublicationCheckDeployed,
		models.PublicationCheckAPIKeySecurity, models.PublicationCheckCORSEnabled,
	}, ids)
}

func TestAgentPublicationGetIgnoresAPublicationFromAnotherEnvironment(t *testing.T) {
	f := newPublicationFixture(t)
	f.repo.GetByAgentFunc = func(_ context.Context, _, _, _ string) (*models.AgentPublication, error) {
		return &models.AgentPublication{EnvironmentUUID: uuid.New(), EnvironmentName: "production-eu"}, nil
	}

	status, err := f.svc().GetPublication(context.Background(), pubOUID, pubProject, pubAgent, pubEnv)
	require.NoError(t, err)
	assert.Nil(t, status.Publication)
}

func TestAgentPublicationEligibilityNamesEachFailedCheck(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(f *publicationFixture)
		failed []string
	}{
		{
			"non-production environment", func(f *publicationFixture) { f.env.IsProduction = false },
			[]string{models.PublicationCheckProductionEnvironment},
		},
		{
			"custom-api agent", func(f *publicationFixture) { f.agent.Type.SubType = string(utils.AgentSubTypeCustomAPI) },
			[]string{models.PublicationCheckChatAPI},
		},
		{"external agent", func(f *publicationFixture) {
			f.agent.Provisioning.Type = string(utils.ExternalAgent)
			f.oc.GetReleaseBindingRolloutFunc = nil // an external agent has no release binding to read
		}, []string{models.PublicationCheckChatAPI, models.PublicationCheckDeployed}},
		{
			"undeployed or suspended", func(f *publicationFixture) { f.deployed = false },
			[]string{models.PublicationCheckDeployed},
		},
		{
			"API-key security off", func(f *publicationFixture) { f.config.EnableApiKeySecurity = false },
			[]string{models.PublicationCheckAPIKeySecurity},
		},
		{"OAuth security", func(f *publicationFixture) {
			f.config.EnableApiKeySecurity, f.config.EnableOAuthSecurity = false, true
		}, []string{models.PublicationCheckAPIKeySecurity}},
		{
			"CORS disabled", func(f *publicationFixture) { f.config.CORSEnabled = false },
			[]string{models.PublicationCheckCORSEnabled},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newPublicationFixture(t)
			tc.mutate(f)

			status, err := f.svc().GetPublication(context.Background(), pubOUID, pubProject, pubAgent, pubEnv)
			require.NoError(t, err)
			assert.False(t, status.Eligible())
			for _, c := range status.Checks {
				shouldFail := false
				for _, id := range tc.failed {
					shouldFail = shouldFail || c.ID == id
				}
				assert.Equal(t, !shouldFail, c.Passed, c.ID)
				if shouldFail {
					assert.NotEmpty(t, c.Reason, "a failed check says why: %s", c.ID)
				}
			}
		})
	}
}

func TestAgentPublicationEligibilityUsesDefaultsWhenTheEnvironmentHasNoSettingsYet(t *testing.T) {
	f := newPublicationFixture(t)
	f.config = nil

	status, err := f.svc().GetPublication(context.Background(), pubOUID, pubProject, pubAgent, pubEnv)
	require.NoError(t, err)
	assert.True(t, checkByID(t, status.Checks, models.PublicationCheckAPIKeySecurity).Passed, "API-key security is on by default")
	assert.True(t, checkByID(t, status.Checks, models.PublicationCheckCORSEnabled).Passed, "CORS is on by default")
}

func TestAgentPublicationGetDoesNotMaskLookupErrors(t *testing.T) {
	boom := errors.New("openchoreo unavailable")
	t.Run("deployment state", func(t *testing.T) {
		f := newPublicationFixture(t)
		f.oc.GetReleaseBindingRolloutFunc = func(_ context.Context, _, _, _ string) (client.ReleaseBindingRollout, error) {
			return client.ReleaseBindingRollout{}, boom
		}
		_, err := f.svc().GetPublication(context.Background(), pubOUID, pubProject, pubAgent, pubEnv)
		assert.ErrorIs(t, err, boom, "a failed lookup is an error, not a failed check")
	})
	t.Run("publication", func(t *testing.T) {
		f := newPublicationFixture(t)
		f.repo.GetByAgentFunc = func(_ context.Context, _, _, _ string) (*models.AgentPublication, error) {
			return nil, boom
		}
		_, err := f.svc().GetPublication(context.Background(), pubOUID, pubProject, pubAgent, pubEnv)
		assert.ErrorIs(t, err, boom)
		assert.NotErrorIs(t, err, utils.ErrAgentNotPublished)
	})
	t.Run("missing agent", func(t *testing.T) {
		f := newPublicationFixture(t)
		f.oc.GetComponentFunc = func(_ context.Context, _, _, _ string) (*models.AgentResponse, error) {
			return nil, utils.ErrNotFound
		}
		_, err := f.svc().GetPublication(context.Background(), pubOUID, pubProject, pubAgent, pubEnv)
		assert.ErrorIs(t, err, utils.ErrAgentNotFound)
	})
}

// ---- Publish ----

func TestPublishAddsThePortalOriginWhenItIsMissing(t *testing.T) {
	f := newPublicationFixture(t)
	f.allowCORSChanges()
	req := models.AgentPublicationRequest{DisplayName: "  HR Helper  ", Tags: []string{" HR ", "Benefits"}}

	pub, err := f.svc().Publish(publisherCtx(t), pubOUID, pubProject, pubAgent, pubEnv, req)
	require.NoError(t, err)
	assert.NotEqual(t, uuid.Nil, pub.ID)
	assert.Equal(t, [][]string{{pubOwnerOrigin, pubPortalOrigin}}, f.deploy.origins, "the owner's origins are kept")

	require.Len(t, f.upserted, 1)
	saved := f.upserted[0]
	assert.True(t, saved.CORSOriginAdded)
	assert.Equal(t, pubPublisherSub, saved.PublishedBy)
	assert.Equal(t, pubEnvUUID, saved.EnvironmentUUID)
	assert.Equal(t, pubOUID, saved.OUID)
	assert.Equal(t, "HR Helper", saved.DisplayName, "the listing is trimmed")
	assert.Equal(t, []string{"HR", "Benefits"}, saved.Tags)
}

func TestPublishLeavesCORSAloneWhenThePortalIsAlreadyAllowed(t *testing.T) {
	for name, origins := range map[string][]string{
		"exact origin":          {pubOwnerOrigin, pubPortalOrigin},
		"origin with a slash":   {pubPortalOrigin + "/"},
		"wildcard":              {"*"},
		"republish, same state": {pubPortalOrigin},
	} {
		t.Run(name, func(t *testing.T) {
			f := newPublicationFixture(t)
			f.config.CORSAllowOrigins = origins // UpdateAgentDeploySettingsFunc stays nil: no restart

			_, err := f.svc().Publish(publisherCtx(t), pubOUID, pubProject, pubAgent, pubEnv, validListing())
			require.NoError(t, err)
			require.Len(t, f.upserted, 1)
			assert.False(t, f.upserted[0].CORSOriginAdded, "publishing did not add the origin, so unpublish must not remove it")
		})
	}
}

func TestPublishRefusesWhenThePortalIsNotConfigured(t *testing.T) {
	// No mocked calls: nothing is looked up or changed.
	svc := NewAgentPublicationService(&repomocks.AgentPublicationRepositoryMock{}, &repomocks.AgentConfigRepositoryMock{},
		&clientmocks.OpenChoreoClientMock{}, &deploySettingsRecorder{t: t}, "", discardLogger())

	_, err := svc.Publish(publisherCtx(t), pubOUID, pubProject, pubAgent, pubEnv, validListing())
	verr := utils.IsValidationError(err)
	require.NotNil(t, verr, "a 400, not a 500: %v", err)
	assert.Equal(t, "The Agent Portal is not configured on this platform", verr.Message)
}

func TestPublishValidatesTheListingBeforeLookingAnythingUp(t *testing.T) {
	long := func(n int) string {
		b := make([]byte, n)
		for i := range b {
			b[i] = 'a'
		}
		return string(b)
	}
	tooManyTags := []string{"a", "b", "c", "d", "e", "f", "g", "h", "i", "j", "k"}
	tests := map[string]models.AgentPublicationRequest{
		"blank display name":  {DisplayName: "   "},
		"long display name":   {DisplayName: long(101)},
		"long description":    {DisplayName: "HR", Description: long(1001)},
		"http icon":           {DisplayName: "HR", IconURL: "http://cdn.example.com/icon.png"},
		"relative icon":       {DisplayName: "HR", IconURL: "/icon.png"},
		"tag with a symbol":   {DisplayName: "HR", Tags: []string{"H&R"}},
		"tag starting with -": {DisplayName: "HR", Tags: []string{"-HR"}},
		"long tag":            {DisplayName: "HR", Tags: []string{long(33)}},
		"duplicate tag":       {DisplayName: "HR", Tags: []string{"HR", " HR"}},
		"too many tags":       {DisplayName: "HR", Tags: tooManyTags},
	}
	for name, req := range tests {
		t.Run(name, func(t *testing.T) {
			f := newPublicationFixture(t)
			f.oc = &clientmocks.OpenChoreoClientMock{} // any lookup panics

			_, err := f.svc().Publish(publisherCtx(t), pubOUID, pubProject, pubAgent, pubEnv, req)
			assert.NotNil(t, utils.IsValidationError(err), "want a validation error, got %v", err)
			assert.Empty(t, f.upserted)
		})
	}
}

func TestPublishRefusesAnIneligibleAgentNamingTheCheck(t *testing.T) {
	f := newPublicationFixture(t)
	f.config.CORSEnabled = false

	_, err := f.svc().Publish(publisherCtx(t), pubOUID, pubProject, pubAgent, pubEnv, validListing())
	verr := utils.IsValidationError(err)
	require.NotNil(t, verr, "want a validation error, got %v", err)
	assert.Contains(t, verr.Message, "CORS must be enabled")
	assert.Empty(t, f.upserted)
	assert.Empty(t, f.deploy.origins)
}

func TestPublishRequiresTheProductionTier(t *testing.T) {
	f := newPublicationFixture(t)
	ctx := jwtassertion.ContextWithTokenClaimsAndScope(auditableCtx(t), &jwtassertion.TokenClaims{
		Sub: pubPublisherSub, OuId: pubOUID, Scope: audit.ScopesOf([]rbac.Permission{rbac.AgentEnvNonProduction}),
	})

	_, err := f.svc().Publish(ctx, pubOUID, pubProject, pubAgent, pubEnv, validListing())
	assert.ErrorIs(t, err, utils.ErrForbidden)
	assert.Empty(t, f.upserted)
	assert.Empty(t, f.deploy.origins)
}

func TestPublishFromAnotherEnvironmentConflictsBeforeTouchingCORS(t *testing.T) {
	f := newPublicationFixture(t)
	f.repo.GetByAgentFunc = func(_ context.Context, _, _, _ string) (*models.AgentPublication, error) {
		return &models.AgentPublication{EnvironmentUUID: uuid.New(), EnvironmentName: "production-eu"}, nil
	}
	f.repo.UpsertFunc = nil // must not be reached

	_, err := f.svc().Publish(publisherCtx(t), pubOUID, pubProject, pubAgent, pubEnv, validListing())
	assert.ErrorIs(t, err, utils.ErrAgentPublishedElsewhere)
	assert.Contains(t, err.Error(), "production-eu", "the owner is told where it is published")
	assert.Empty(t, f.deploy.origins, "a doomed publish does not restart the agent")
}

func TestPublishThatLosesTheRaceRemovesTheOriginItAdded(t *testing.T) {
	f := newPublicationFixture(t)
	f.allowCORSChanges()
	f.repo.UpsertFunc = func(_ context.Context, _ *models.AgentPublication) (*models.AgentPublication, error) {
		return nil, repositories.ErrAgentPublishedFromOtherEnvironment
	}

	_, err := f.svc().Publish(publisherCtx(t), pubOUID, pubProject, pubAgent, pubEnv, validListing())
	assert.ErrorIs(t, err, utils.ErrAgentPublishedElsewhere)
	assert.Equal(t, [][]string{{pubOwnerOrigin, pubPortalOrigin}, {pubOwnerOrigin}}, f.deploy.origins,
		"the origin is added, then taken back out")
}

func TestPublishDoesNotMaskARepositoryError(t *testing.T) {
	f := newPublicationFixture(t)
	boom := errors.New("database down")
	f.repo.GetByAgentFunc = func(_ context.Context, _, _, _ string) (*models.AgentPublication, error) {
		return nil, boom
	}

	_, err := f.svc().Publish(publisherCtx(t), pubOUID, pubProject, pubAgent, pubEnv, validListing())
	assert.ErrorIs(t, err, boom)
	assert.NotErrorIs(t, err, utils.ErrAgentPublishedElsewhere)
	assert.Empty(t, f.deploy.origins)
}

func TestPublishReportsAFailedCORSUpdateWithoutListing(t *testing.T) {
	f := newPublicationFixture(t)
	boom := errors.New("release binding update failed")
	f.deploy.UpdateAgentDeploySettingsFunc = func(*spec.UpdateAgentDeploySettingsRequest) error { return boom }

	_, err := f.svc().Publish(publisherCtx(t), pubOUID, pubProject, pubAgent, pubEnv, validListing())
	assert.ErrorIs(t, err, boom)
	assert.Empty(t, f.upserted, "an agent the portal cannot reach is not listed")
}

// A caller allowed to publish, on a context with no audit recorder.
func unrecordedPublisherCtx() context.Context {
	return jwtassertion.ContextWithTokenClaimsAndScope(context.Background(), &jwtassertion.TokenClaims{
		Sub:   pubPublisherSub,
		OuId:  pubOUID,
		Scope: audit.ScopesOf([]rbac.Permission{rbac.AgentEnvNonProduction, rbac.AgentEnvProduction}),
	})
}

func TestPublishIsRefusedWhenItCannotBeRecorded(t *testing.T) {
	f := newPublicationFixture(t) // CORS changes stay disallowed
	f.repo.UpsertFunc = nil       // must not be reached

	_, err := f.svc().Publish(unrecordedPublisherCtx(), pubOUID, pubProject, pubAgent, pubEnv, validListing())
	assert.ErrorIs(t, err, audit.ErrRecorderUnavailable)
}

func TestUnpublishIsRefusedWhenItCannotBeRecorded(t *testing.T) {
	f := newPublicationFixture(t) // DeleteFunc stays nil: nothing may be removed

	err := f.svc().Unpublish(unrecordedPublisherCtx(), pubOUID, pubProject, pubAgent, pubEnv)
	assert.ErrorIs(t, err, audit.ErrRecorderUnavailable)
}

// ---- Unpublish ----

func (f *publicationFixture) deleteReturns(row *models.AgentPublication, err error) {
	f.repo.DeleteFunc = func(_ context.Context, ouID, _, _ string, envUUID uuid.UUID) (*models.AgentPublication, error) {
		assert.Equal(f.t, pubOUID, ouID)
		assert.Equal(f.t, pubEnvUUID, envUUID, "only this environment's publication is removed")
		return row, err
	}
}

func TestUnpublishRemovesTheOriginPublishingAdded(t *testing.T) {
	f := newPublicationFixture(t)
	f.allowCORSChanges()
	f.config.CORSAllowOrigins = []string{pubOwnerOrigin, pubPortalOrigin}
	f.deleteReturns(&models.AgentPublication{CORSOriginAdded: true}, nil)

	require.NoError(t, f.svc().Unpublish(publisherCtx(t), pubOUID, pubProject, pubAgent, pubEnv))
	assert.Equal(t, [][]string{{pubOwnerOrigin}}, f.deploy.origins)
}

func TestUnpublishLeavesAnOriginTheOwnerAdded(t *testing.T) {
	f := newPublicationFixture(t)
	f.config.CORSAllowOrigins = []string{pubOwnerOrigin, pubPortalOrigin} // CORS must not change
	f.deleteReturns(&models.AgentPublication{CORSOriginAdded: false}, nil)

	require.NoError(t, f.svc().Unpublish(publisherCtx(t), pubOUID, pubProject, pubAgent, pubEnv))
}

func TestUnpublishSucceedsWhenTheOriginCannotBeRemoved(t *testing.T) {
	t.Run("the CORS update fails", func(t *testing.T) {
		f := newPublicationFixture(t)
		f.config.CORSAllowOrigins = []string{pubOwnerOrigin, pubPortalOrigin}
		f.deploy.UpdateAgentDeploySettingsFunc = func(*spec.UpdateAgentDeploySettingsRequest) error {
			return errors.New("release binding update failed")
		}
		f.deleteReturns(&models.AgentPublication{CORSOriginAdded: true}, nil)

		assert.NoError(t, f.svc().Unpublish(publisherCtx(t), pubOUID, pubProject, pubAgent, pubEnv),
			"the listing is gone, so the unpublish succeeded")
		assert.Len(t, f.deploy.origins, 1, "the removal was attempted")
	})
	t.Run("the portal is the only origin", func(t *testing.T) {
		f := newPublicationFixture(t)
		f.config.CORSAllowOrigins = []string{pubPortalOrigin} // an empty list cannot be saved: CORS must not change
		f.deleteReturns(&models.AgentPublication{CORSOriginAdded: true}, nil)

		assert.NoError(t, f.svc().Unpublish(publisherCtx(t), pubOUID, pubProject, pubAgent, pubEnv))
	})
}

func TestUnpublishReportsAnAgentNotPublishedHere(t *testing.T) {
	f := newPublicationFixture(t)
	f.deleteReturns(nil, repositories.ErrAgentPublicationNotFound)

	err := f.svc().Unpublish(publisherCtx(t), pubOUID, pubProject, pubAgent, pubEnv)
	assert.ErrorIs(t, err, utils.ErrAgentNotPublished)
}

func TestUnpublishDoesNotMaskARepositoryErrorAsNotPublished(t *testing.T) {
	f := newPublicationFixture(t)
	boom := errors.New("database down")
	f.deleteReturns(nil, boom)

	err := f.svc().Unpublish(publisherCtx(t), pubOUID, pubProject, pubAgent, pubEnv)
	assert.ErrorIs(t, err, boom)
	assert.NotErrorIs(t, err, utils.ErrAgentNotPublished)
}

func TestUnpublishRequiresTheProductionTier(t *testing.T) {
	f := newPublicationFixture(t) // DeleteFunc stays nil: nothing may be removed
	ctx := jwtassertion.ContextWithTokenClaimsAndScope(auditableCtx(t), &jwtassertion.TokenClaims{
		Sub: pubPublisherSub, OuId: pubOUID, Scope: audit.ScopesOf([]rbac.Permission{rbac.AgentEnvNonProduction}),
	})

	err := f.svc().Unpublish(ctx, pubOUID, pubProject, pubAgent, pubEnv)
	assert.ErrorIs(t, err, utils.ErrForbidden)
}
