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
	"fmt"
	"log/slog"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/wso2/agent-manager/agent-manager-service/clients/openchoreosvc/client"
	"github.com/wso2/agent-manager/agent-manager-service/middleware/jwtassertion"
	"github.com/wso2/agent-manager/agent-manager-service/models"
	"github.com/wso2/agent-manager/agent-manager-service/repositories"
	"github.com/wso2/agent-manager/agent-manager-service/spec"
	"github.com/wso2/agent-manager/agent-manager-service/utils"
)

// Listing limits, as the publication API documents them.
const (
	publicationDisplayNameMaxLen = 100
	publicationDescriptionMaxLen = 1000
	publicationIconURLMaxLen     = 2048
	publicationMaxTags           = 10
	publicationTagMaxLen         = 32
)

var publicationTagPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9 _-]*$`)

// AgentDeploySettingsUpdater is the part of the agent service publishing needs: it changes an
// agent's CORS through the same path, and with the same pod rollout, as the deploy-settings save.
type AgentDeploySettingsUpdater interface {
	UpdateAgentDeploySettings(ctx context.Context, ouID, projectName, agentName string, req *spec.UpdateAgentDeploySettingsRequest) error
}

// AgentPublicationService publishes agents to the Agent Portal from a production environment.
type AgentPublicationService interface {
	// GetPublication returns the agent's publication from the environment, if any, and the
	// eligibility checks. Reading needs no environment-tier permission.
	GetPublication(ctx context.Context, ouID, projectName, agentName, envName string) (*models.AgentPublicationStatus, error)
	// Publish lists the agent, or updates its listing, adding the portal origin to its CORS when missing.
	Publish(ctx context.Context, ouID, projectName, agentName, envName string, req models.AgentPublicationRequest) (*models.AgentPublication, error)
	// Unpublish removes the listing, and the portal origin when publishing added it.
	Unpublish(ctx context.Context, ouID, projectName, agentName, envName string) error
}

type agentPublicationService struct {
	repo            repositories.AgentPublicationRepository
	agentConfigRepo repositories.AgentConfigRepository
	ocClient        client.OpenChoreoClient
	deploySettings  AgentDeploySettingsUpdater
	// portalOrigin is PORTAL_PUBLIC_ORIGIN; empty when no portal is deployed.
	portalOrigin string
	logger       *slog.Logger
}

// NewAgentPublicationService creates an AgentPublicationService.
func NewAgentPublicationService(
	repo repositories.AgentPublicationRepository,
	agentConfigRepo repositories.AgentConfigRepository,
	ocClient client.OpenChoreoClient,
	deploySettings AgentDeploySettingsUpdater,
	portalOrigin string,
	logger *slog.Logger,
) AgentPublicationService {
	return &agentPublicationService{
		repo:            repo,
		agentConfigRepo: agentConfigRepo,
		ocClient:        ocClient,
		deploySettings:  deploySettings,
		portalOrigin:    portalOrigin,
		logger:          logger,
	}
}

func (s *agentPublicationService) GetPublication(ctx context.Context, ouID, projectName, agentName, envName string) (*models.AgentPublicationStatus, error) {
	agent, err := getAgent(ctx, s.ocClient, ouID, projectName, agentName)
	if err != nil {
		return nil, err
	}
	env, err := s.ocClient.GetEnvironment(ctx, ouID, envName)
	if err != nil {
		return nil, translateEnvironmentError(err)
	}
	envUUID, err := parseEnvUUID(env)
	if err != nil {
		return nil, err
	}
	checks, _, err := s.eligibility(ctx, ouID, projectName, agent, env)
	if err != nil {
		return nil, err
	}
	status := &models.AgentPublicationStatus{Checks: checks}

	pub, err := s.repo.GetByAgent(ctx, ouID, projectName, agentName)
	switch {
	case errors.Is(err, repositories.ErrAgentPublicationNotFound):
	case err != nil:
		return nil, fmt.Errorf("failed to read the agent's publication: %w", err)
	case pub.EnvironmentUUID == envUUID:
		// A publication from another environment is not this environment's.
		status.Publication = pub
	}
	return status, nil
}

func (s *agentPublicationService) Publish(
	ctx context.Context, ouID, projectName, agentName, envName string, req models.AgentPublicationRequest,
) (*models.AgentPublication, error) {
	req, err := normalizePublicationRequest(req)
	if err != nil {
		return nil, err
	}
	if s.portalOrigin == "" {
		return nil, utils.NewValidationError("The Agent Portal is not configured on this platform",
			"PORTAL_PUBLIC_ORIGIN is not set")
	}
	agent, err := getAgent(ctx, s.ocClient, ouID, projectName, agentName)
	if err != nil {
		return nil, err
	}
	// Loads the environment and refuses a caller without its tier: agent:env-production here.
	env, err := requireEnvironmentTier(ctx, s.ocClient, s.logger, ouID, envName)
	if err != nil {
		return nil, err
	}
	envUUID, err := parseEnvUUID(env)
	if err != nil {
		return nil, err
	}
	checks, apiCfg, err := s.eligibility(ctx, ouID, projectName, agent, env)
	if err != nil {
		return nil, err
	}
	if failed := models.FirstFailedPublicationCheck(checks); failed != nil {
		return nil, utils.NewValidationErrorf(failed.Reason, "eligibility check %s failed", failed.ID)
	}

	// An early exit only, so a doomed publish does not restart the agent's pods; the upsert below decides.
	switch existing, err := s.repo.GetByAgent(ctx, ouID, projectName, agentName); {
	case errors.Is(err, repositories.ErrAgentPublicationNotFound):
	case err != nil:
		return nil, fmt.Errorf("failed to read the agent's publication: %w", err)
	case existing.EnvironmentUUID != envUUID:
		return nil, publishedElsewhere(existing.EnvironmentName)
	}

	originAdded := false
	if !originAllowed(apiCfg.CORSAllowOrigins, s.portalOrigin) {
		origins := append(slices.Clone(apiCfg.CORSAllowOrigins), s.portalOrigin)
		if err := s.setCORSOrigins(ctx, ouID, projectName, agentName, envName, origins); err != nil {
			return nil, fmt.Errorf("failed to add the Agent Portal origin to the agent's CORS: %w", err)
		}
		originAdded = true
	}

	pub, err := s.repo.Upsert(ctx, &models.AgentPublication{
		OUID:            ouID,
		ProjectName:     projectName,
		AgentName:       agentName,
		EnvironmentUUID: envUUID,
		EnvironmentName: env.Name,
		DisplayName:     req.DisplayName,
		Description:     req.Description,
		IconURL:         req.IconURL,
		Tags:            req.Tags,
		CORSOriginAdded: originAdded,
		PublishedBy:     jwtassertion.CallerSubject(ctx),
	})
	if err != nil {
		if originAdded {
			// Nothing was listed, so the origin this call added must not outlive it.
			s.removePortalOrigin(ctx, ouID, projectName, agentName, envName)
		}
		if errors.Is(err, repositories.ErrAgentPublishedFromOtherEnvironment) {
			// Lost a race with a publish from another environment.
			return nil, publishedElsewhere("")
		}
		return nil, fmt.Errorf("failed to save the publication: %w", err)
	}
	s.logger.Info("Agent published to the Agent Portal", "ouID", ouID, "projectName", projectName,
		"agentName", agentName, "environment", env.Name, "corsOriginAdded", originAdded)
	return pub, nil
}

func (s *agentPublicationService) Unpublish(ctx context.Context, ouID, projectName, agentName, envName string) error {
	if _, err := getAgent(ctx, s.ocClient, ouID, projectName, agentName); err != nil {
		return err
	}
	env, err := requireEnvironmentTier(ctx, s.ocClient, s.logger, ouID, envName)
	if err != nil {
		return err
	}
	envUUID, err := parseEnvUUID(env)
	if err != nil {
		return err
	}
	removed, err := s.repo.Delete(ctx, ouID, projectName, agentName, envUUID)
	if errors.Is(err, repositories.ErrAgentPublicationNotFound) {
		return utils.ErrAgentNotPublished
	}
	if err != nil {
		return fmt.Errorf("failed to remove the publication: %w", err)
	}
	// Best effort: the listing is gone and no new keys are issued, so a leftover origin is harmless.
	if removed.CORSOriginAdded {
		s.removePortalOrigin(ctx, ouID, projectName, agentName, envName)
	}
	s.logger.Info("Agent unpublished from the Agent Portal", "ouID", ouID, "projectName", projectName,
		"agentName", agentName, "environment", env.Name)
	return nil
}

// eligibility runs the publication checks and returns the agent's effective API settings in the
// environment, the ones its next deploy would use.
func (s *agentPublicationService) eligibility(
	ctx context.Context, ouID, projectName string, agent *models.AgentResponse, env *models.EnvironmentResponse,
) ([]models.AgentPublicationCheck, resolvedCORSConfig, error) {
	checks := make([]models.AgentPublicationCheck, 0, 5)
	add := func(id string, passed bool, reason string) {
		if passed {
			reason = ""
		}
		checks = append(checks, models.AgentPublicationCheck{ID: id, Passed: passed, Reason: reason})
	}

	add(models.PublicationCheckProductionEnvironment, env.IsProduction,
		"Only production environments can be published to the Agent Portal.")

	internal := !isExternalProvisioned(agent)
	add(models.PublicationCheckChatAPI,
		internal && agent.Type.Type == string(utils.AgentTypeAPI) && agent.Type.SubType == string(utils.AgentSubTypeChatAPI),
		"Only internal agents with the chat-api interface can be published to the Agent Portal.")

	if internal {
		rollout, err := s.ocClient.GetReleaseBindingRollout(ctx, ouID, agent.Name, env.Name)
		if err != nil {
			return nil, resolvedCORSConfig{}, fmt.Errorf("failed to read the agent's deployment in %s: %w", env.Name, err)
		}
		add(models.PublicationCheckDeployed, rollout.Deployed,
			"The agent is not deployed in this environment, or it is suspended. Deploy it first.")
	} else {
		add(models.PublicationCheckDeployed, false, "External agents are not deployed by the platform.")
	}

	var existing *models.AgentConfig
	switch cfg, err := s.agentConfigRepo.Get(ctx, ouID, projectName, agent.Name, env.Name); {
	case errors.Is(err, repositories.ErrAgentConfigNotFound):
	case err != nil:
		return nil, resolvedCORSConfig{}, fmt.Errorf("failed to read the agent's settings in %s: %w", env.Name, err)
	default:
		existing = cfg
	}
	apiCfg := resolveAPIConfig(existing, nil, nil, nil, nil, true)
	add(models.PublicationCheckAPIKeySecurity, apiCfg.EnableApiKeySecurity && !apiCfg.EnableOAuthSecurity,
		"API-key security must be on. Turn it on in the agent's deploy settings for this environment.")
	add(models.PublicationCheckCORSEnabled, apiCfg.CORSEnabled,
		"CORS must be enabled. Turn it on in the agent's deploy settings for this environment.")
	return checks, apiCfg, nil
}

// setCORSOrigins replaces the agent's CORS allowed origins in the environment, keeping every
// other setting. It rolls the agent's pods, as any deploy-settings save does.
func (s *agentPublicationService) setCORSOrigins(ctx context.Context, ouID, projectName, agentName, envName string, origins []string) error {
	return s.deploySettings.UpdateAgentDeploySettings(ctx, ouID, projectName, agentName, &spec.UpdateAgentDeploySettingsRequest{
		EnvironmentName: envName,
		CorsConfig:      &spec.CORSConfig{AllowOrigin: origins},
	})
}

// removePortalOrigin takes the portal origin back out of the agent's CORS. It never fails the
// caller: a leftover origin only lets the portal's browser tabs reach an agent they hold no key for.
func (s *agentPublicationService) removePortalOrigin(ctx context.Context, ouID, projectName, agentName, envName string) {
	log := s.logger.With("ouID", ouID, "projectName", projectName, "agentName", agentName, "environment", envName)
	cfg, err := s.agentConfigRepo.Get(ctx, ouID, projectName, agentName, envName)
	if err != nil {
		log.Error("Could not read the agent's CORS to remove the Agent Portal origin; it is left in place", "error", err)
		return
	}
	origins := slices.DeleteFunc(slices.Clone(cfg.CORSAllowOrigins), func(o string) bool { return o == s.portalOrigin })
	if len(origins) == len(cfg.CORSAllowOrigins) {
		return
	}
	if len(origins) == 0 {
		// The deploy-settings merge reads an empty list as "no change", so it cannot be emptied.
		log.Warn("The Agent Portal origin is the agent's only CORS origin, so it is left in place")
		return
	}
	if err := s.setCORSOrigins(ctx, ouID, projectName, agentName, envName, origins); err != nil {
		log.Error("Could not remove the Agent Portal origin from the agent's CORS; it is left in place", "error", err)
	}
}

// originAllowed is true when CORS already lets origin in, by name or by wildcard.
func originAllowed(allowed []string, origin string) bool {
	return slices.ContainsFunc(allowed, func(o string) bool {
		return o == "*" || strings.TrimSuffix(o, "/") == origin
	})
}

func publishedElsewhere(envName string) error {
	if envName == "" {
		return utils.ErrAgentPublishedElsewhere
	}
	return fmt.Errorf("%w (%s); unpublish it there first", utils.ErrAgentPublishedElsewhere, envName)
}

func parseEnvUUID(env *models.EnvironmentResponse) (uuid.UUID, error) {
	id, err := uuid.Parse(env.UUID)
	if err != nil {
		return uuid.Nil, fmt.Errorf("environment %s has an invalid UUID %q: %w", env.Name, env.UUID, err)
	}
	return id, nil
}

// normalizePublicationRequest trims the listing and enforces the API's limits; route validation
// covers path parameters only, so the body is checked here.
func normalizePublicationRequest(req models.AgentPublicationRequest) (models.AgentPublicationRequest, error) {
	req.DisplayName = strings.TrimSpace(req.DisplayName)
	req.Description = strings.TrimSpace(req.Description)
	req.IconURL = strings.TrimSpace(req.IconURL)

	if req.DisplayName == "" || utf8.RuneCountInString(req.DisplayName) > publicationDisplayNameMaxLen {
		return req, utils.NewValidationErrorf(
			fmt.Sprintf("Display name is required and must be at most %d characters", publicationDisplayNameMaxLen),
			"invalid displayName")
	}
	if utf8.RuneCountInString(req.Description) > publicationDescriptionMaxLen {
		return req, utils.NewValidationErrorf(
			fmt.Sprintf("Description must be at most %d characters", publicationDescriptionMaxLen), "invalid description")
	}
	if req.IconURL != "" {
		u, err := url.Parse(req.IconURL)
		if len(req.IconURL) > publicationIconURLMaxLen || err != nil || u.Scheme != "https" || u.Host == "" {
			return req, utils.NewValidationErrorf("Icon URL must be an https URL", "invalid iconUrl")
		}
	}
	if len(req.Tags) > publicationMaxTags {
		return req, utils.NewValidationErrorf(fmt.Sprintf("At most %d tags are allowed", publicationMaxTags), "too many tags")
	}
	tags := make([]string, 0, len(req.Tags))
	for _, tag := range req.Tags {
		tag = strings.TrimSpace(tag)
		if utf8.RuneCountInString(tag) > publicationTagMaxLen || !publicationTagPattern.MatchString(tag) {
			return req, utils.NewValidationErrorf(
				fmt.Sprintf("Tag %q must be 1 to %d letters, digits, spaces, '_' or '-', starting with a letter or digit",
					tag, publicationTagMaxLen), "invalid tag")
		}
		if slices.Contains(tags, tag) {
			return req, utils.NewValidationErrorf(fmt.Sprintf("Tag %q is listed twice", tag), "duplicate tag")
		}
		tags = append(tags, tag)
	}
	req.Tags = tags
	return req, nil
}
