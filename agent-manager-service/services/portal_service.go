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
	"slices"
	"strings"
	"sync"

	"github.com/google/uuid"

	"github.com/wso2/agent-manager/agent-manager-service/clients/openchoreosvc/client"
	"github.com/wso2/agent-manager/agent-manager-service/models"
	"github.com/wso2/agent-manager/agent-manager-service/repositories"
	"github.com/wso2/agent-manager/agent-manager-service/utils"
)

// Catalog paging, as the portal API documents it.
const (
	portalDefaultLimit = 20
	portalMaxLimit     = 100
	// portalAvailabilityLookups bounds the deployment lookups one catalog page runs at once.
	portalAvailabilityLookups = 8
)

// PortalChatKeyIssuer issues the per-user key a portal user chats with an agent through.
type PortalChatKeyIssuer interface {
	IssuePortalChatKey(ctx context.Context, ouID, projectName, agentName, envName, userSub, publicationID string) (*models.IssueTestAPIKeyResponse, error)
}

// AgentEndpointsResolver resolves an agent's browser-reachable endpoint URLs in an environment.
type AgentEndpointsResolver interface {
	GetAgentEndpoints(ctx context.Context, ouID, projectName, agentName, environmentName string) (map[string]models.EndpointsResponse, error)
}

// PortalService serves the Agent Portal: the catalog of published agents and the chat keys
// portal users chat with them through. Chat traffic itself never passes through it.
type PortalService interface {
	// ListAgents returns one page of the organization's published agents, newest update first.
	ListAgents(ctx context.Context, ouID string, filter models.AgentPublicationFilter) (*models.PortalAgentPage, error)
	// GetAgent returns one published agent.
	GetAgent(ctx context.Context, ouID string, publicationID uuid.UUID) (*models.PortalAgent, error)
	// IssueChatKey issues or rotates the user's key for the agent, refusing an unavailable agent.
	IssueChatKey(ctx context.Context, ouID string, publicationID uuid.UUID, userSub string) (*models.PortalChatKey, error)
}

type portalService struct {
	repo            repositories.AgentPublicationRepository
	agentConfigRepo repositories.AgentConfigRepository
	ocClient        client.OpenChoreoClient
	endpoints       AgentEndpointsResolver
	keys            PortalChatKeyIssuer
	// portalOrigin is PORTAL_PUBLIC_ORIGIN; with none, no agent is reachable from a portal.
	portalOrigin string
	logger       *slog.Logger
}

// NewPortalService creates a PortalService.
func NewPortalService(
	repo repositories.AgentPublicationRepository,
	agentConfigRepo repositories.AgentConfigRepository,
	ocClient client.OpenChoreoClient,
	endpoints AgentEndpointsResolver,
	keys PortalChatKeyIssuer,
	portalOrigin string,
	logger *slog.Logger,
) PortalService {
	return &portalService{
		repo:            repo,
		agentConfigRepo: agentConfigRepo,
		ocClient:        ocClient,
		endpoints:       endpoints,
		keys:            keys,
		portalOrigin:    portalOrigin,
		logger:          logger,
	}
}

func (s *portalService) ListAgents(ctx context.Context, ouID string, filter models.AgentPublicationFilter) (*models.PortalAgentPage, error) {
	filter.Search = strings.TrimSpace(filter.Search)
	filter.Tag = strings.TrimSpace(filter.Tag)
	if filter.Limit <= 0 {
		filter.Limit = portalDefaultLimit
	}
	filter.Limit = min(filter.Limit, portalMaxLimit)
	filter.Offset = max(filter.Offset, 0)

	rows, total, err := s.repo.List(ctx, ouID, filter)
	if err != nil {
		return nil, fmt.Errorf("failed to list published agents: %w", err)
	}
	tags, err := s.repo.ListTags(ctx, ouID)
	if err != nil {
		return nil, fmt.Errorf("failed to list catalog tags: %w", err)
	}

	agents := make([]models.PortalAgent, len(rows))
	sem := make(chan struct{}, portalAvailabilityLookups)
	var wg sync.WaitGroup
	for i := range rows {
		agents[i].Publication = rows[i]
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			available, err := s.available(ctx, &rows[i])
			if err != nil {
				// One failed lookup must not hide the whole catalog; the chat-key call checks again.
				s.logger.Warn("Could not check a published agent's availability; listing it as unavailable",
					"ouID", ouID, "publicationId", rows[i].ID, "agentName", rows[i].AgentName, "error", err)
			}
			agents[i].Available = available && err == nil
		}(i)
	}
	wg.Wait()

	return &models.PortalAgentPage{Agents: agents, Tags: tags, Total: total, Limit: filter.Limit, Offset: filter.Offset}, nil
}

func (s *portalService) GetAgent(ctx context.Context, ouID string, publicationID uuid.UUID) (*models.PortalAgent, error) {
	pub, err := s.publication(ctx, ouID, publicationID)
	if err != nil {
		return nil, err
	}
	available, err := s.available(ctx, pub)
	if err != nil {
		s.logger.Warn("Could not check a published agent's availability; showing it as unavailable",
			"ouID", ouID, "publicationId", pub.ID, "agentName", pub.AgentName, "error", err)
	}
	return &models.PortalAgent{Publication: *pub, Available: available && err == nil}, nil
}

func (s *portalService) IssueChatKey(ctx context.Context, ouID string, publicationID uuid.UUID, userSub string) (*models.PortalChatKey, error) {
	if userSub == "" {
		return nil, fmt.Errorf("%w: chat keys are issued to signed-in users only", utils.ErrUnauthorized)
	}
	pub, err := s.publication(ctx, ouID, publicationID)
	if err != nil {
		return nil, err
	}
	// Unlike the catalog, a failed check is an error here: the caller is told, not guessed for.
	available, err := s.available(ctx, pub)
	if err != nil {
		return nil, err
	}
	if !available {
		return nil, utils.ErrPortalAgentUnavailable
	}
	// Resolved before the key, so a failure never leaves a key nobody can use.
	endpoints, err := s.endpoints.GetAgentEndpoints(ctx, ouID, pub.ProjectName, pub.AgentName, pub.EnvironmentName)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve the agent's chat endpoint: %w", err)
	}
	chatURL := chatURLOf(endpoints)
	if chatURL == "" {
		return nil, utils.ErrPortalAgentUnavailable
	}

	key, err := s.keys.IssuePortalChatKey(ctx, ouID, pub.ProjectName, pub.AgentName, pub.EnvironmentName, userSub, pub.ID.String())
	if err != nil {
		return nil, err
	}
	return &models.PortalChatKey{
		APIKey:           key.APIKey,
		ExpiresAt:        key.ExpiresAt,
		GatewayConnected: key.GatewayConnected != nil && *key.GatewayConnected,
		ChatURL:          chatURL,
	}, nil
}

func (s *portalService) publication(ctx context.Context, ouID string, publicationID uuid.UUID) (*models.AgentPublication, error) {
	pub, err := s.repo.GetByID(ctx, ouID, publicationID)
	if errors.Is(err, repositories.ErrAgentPublicationNotFound) {
		return nil, utils.ErrPortalAgentNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("failed to read the published agent: %w", err)
	}
	return pub, nil
}

// available is whether portal users can chat with the agent now: it is deployed and not
// suspended, and its settings still let the portal's browser tabs in with an API key.
func (s *portalService) available(ctx context.Context, pub *models.AgentPublication) (bool, error) {
	if s.portalOrigin == "" {
		return false, nil
	}
	rollout, err := s.ocClient.GetReleaseBindingRollout(ctx, pub.OUID, pub.AgentName, pub.EnvironmentName)
	if err != nil {
		return false, fmt.Errorf("failed to read the agent's deployment in %s: %w", pub.EnvironmentName, err)
	}
	if !rollout.Deployed {
		return false, nil
	}
	var existing *models.AgentConfig
	switch cfg, err := s.agentConfigRepo.Get(ctx, pub.OUID, pub.ProjectName, pub.AgentName, pub.EnvironmentName); {
	case errors.Is(err, repositories.ErrAgentConfigNotFound):
	case err != nil:
		return false, fmt.Errorf("failed to read the agent's settings in %s: %w", pub.EnvironmentName, err)
	default:
		existing = cfg
	}
	apiCfg := resolveAPIConfig(existing, nil, nil, nil, nil, true)
	return apiCfg.EnableApiKeySecurity && !apiCfg.EnableOAuthSecurity &&
		apiCfg.CORSEnabled && originAllowed(apiCfg.CORSAllowOrigins, s.portalOrigin), nil
}

// chatURLOf is the chat-api URL of the agent's first endpoint by name, as console Try-It picks it.
func chatURLOf(endpoints map[string]models.EndpointsResponse) string {
	names := make([]string, 0, len(endpoints))
	for name, ep := range endpoints {
		if ep.URL != "" {
			names = append(names, name)
		}
	}
	if len(names) == 0 {
		return ""
	}
	slices.Sort(names)
	return strings.TrimSuffix(endpoints[names[0]].URL, "/") + "/chat"
}
