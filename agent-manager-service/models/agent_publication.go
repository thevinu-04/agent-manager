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

package models

import (
	"time"

	"github.com/google/uuid"
)

// AgentPublication lists an agent in the Agent Portal. An agent has at most one, in the production
// environment it was published from.
type AgentPublication struct {
	ID              uuid.UUID `gorm:"column:id;primaryKey;type:uuid;default:gen_random_uuid()"`
	OUID            string    `gorm:"column:ou_id;not null"`
	ProjectName     string    `gorm:"column:project_name;not null"`
	AgentName       string    `gorm:"column:agent_name;not null"`
	EnvironmentUUID uuid.UUID `gorm:"column:environment_uuid;type:uuid;not null"`
	EnvironmentName string    `gorm:"column:environment_name;not null"`
	DisplayName     string    `gorm:"column:display_name;not null"`
	Description     string    `gorm:"column:description;not null;default:''"`
	IconURL         string    `gorm:"column:icon_url;not null;default:''"`
	Tags            []string  `gorm:"column:tags;type:jsonb;serializer:json;not null;default:'[]'"`
	// CORSOriginAdded is true when publishing added the portal origin to the agent's CORS, so
	// unpublishing removes only an origin it added.
	CORSOriginAdded bool `gorm:"column:cors_origin_added;not null;default:false"`
	// PublishedBy is the user ID of whoever last published or updated the publication.
	PublishedBy string    `gorm:"column:published_by;not null"`
	CreatedAt   time.Time `gorm:"column:created_at;not null;default:NOW()"`
	UpdatedAt   time.Time `gorm:"column:updated_at;not null;default:NOW()"`
}

func (AgentPublication) TableName() string { return "agent_publications" }

// AgentPublicationRequest is how an owner wants the agent listed in the portal catalog.
type AgentPublicationRequest struct {
	DisplayName string
	Description string
	IconURL     string
	Tags        []string
}

// Eligibility check IDs, as the publication API reports them.
const (
	PublicationCheckProductionEnvironment = "PRODUCTION_ENVIRONMENT"
	PublicationCheckChatAPI               = "CHAT_API"
	PublicationCheckDeployed              = "DEPLOYED"
	PublicationCheckAPIKeySecurity        = "API_KEY_SECURITY"
	PublicationCheckCORSEnabled           = "CORS_ENABLED"
)

// AgentPublicationCheck is one requirement an agent must meet to be published. Reason says, in
// words an owner can act on, why it failed; it is empty when the check passed.
type AgentPublicationCheck struct {
	ID     string
	Passed bool
	Reason string
}

// AgentPublicationStatus is whether the agent is published from one environment, and whether it could be.
type AgentPublicationStatus struct {
	// Publication is nil when the agent is not published from the environment.
	Publication *AgentPublication
	Checks      []AgentPublicationCheck
}

// Eligible is true when every check passed.
func (s *AgentPublicationStatus) Eligible() bool {
	return FirstFailedPublicationCheck(s.Checks) == nil
}

// FirstFailedPublicationCheck returns the first check that failed, or nil when all passed.
func FirstFailedPublicationCheck(checks []AgentPublicationCheck) *AgentPublicationCheck {
	for i := range checks {
		if !checks[i].Passed {
			return &checks[i]
		}
	}
	return nil
}

// PortalAgent is a published agent as portal users see it.
type PortalAgent struct {
	Publication AgentPublication
	// Available is false while the agent cannot be chatted with: suspended or undeployed, CORS no
	// longer allowing the portal, or API-key security off.
	Available bool
}

// PortalAgentPage is one page of the portal catalog.
type PortalAgentPage struct {
	Agents []PortalAgent
	// Tags is every tag the organization's publications use, for the tag filter.
	Tags   []string
	Total  int64
	Limit  int
	Offset int
}

// PortalChatKey is what the portal needs to chat with an agent through its gateway.
type PortalChatKey struct {
	APIKey           string
	ExpiresAt        string
	GatewayConnected bool
	// ChatURL is the agent's chat-api endpoint the browser posts to.
	ChatURL string
}

// AgentPublicationFilter narrows the portal catalog. Empty fields match everything.
type AgentPublicationFilter struct {
	// Search is a case-insensitive substring of the display name or description.
	Search string
	// Tag keeps only publications carrying this tag.
	Tag    string
	Limit  int
	Offset int
}
