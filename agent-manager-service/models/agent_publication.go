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

// AgentPublicationFilter narrows the portal catalog. Empty fields match everything.
type AgentPublicationFilter struct {
	// Search is a case-insensitive substring of the display name or description.
	Search string
	// Tag keeps only publications carrying this tag.
	Tag    string
	Limit  int
	Offset int
}
