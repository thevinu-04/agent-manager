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

package dbmigrations

import (
	"gorm.io/gorm"
)

// agent_publications lists agents in the Agent Portal. An agent is published from at most one
// (production) environment, so the unique key leaves the environment out.
var migration060 = migration{
	ID: 60,
	Migrate: func(db *gorm.DB) error {
		return db.Transaction(func(tx *gorm.DB) error {
			createPublications := `
			CREATE TABLE IF NOT EXISTS agent_publications (
				id                UUID PRIMARY KEY DEFAULT gen_random_uuid(),
				ou_id             VARCHAR(255) NOT NULL,
				project_name      VARCHAR(255) NOT NULL,
				agent_name        VARCHAR(255) NOT NULL,
				environment_uuid  UUID NOT NULL,
				environment_name  VARCHAR(255) NOT NULL,
				display_name      VARCHAR(255) NOT NULL,
				description       TEXT NOT NULL DEFAULT '',
				icon_url          TEXT NOT NULL DEFAULT '',
				tags              JSONB NOT NULL DEFAULT '[]',
				cors_origin_added BOOLEAN NOT NULL DEFAULT FALSE,
				published_by      VARCHAR(255) NOT NULL,
				created_at        TIMESTAMPTZ NOT NULL DEFAULT NOW(),
				updated_at        TIMESTAMPTZ NOT NULL DEFAULT NOW(),

				CONSTRAINT uq_agent_publications_agent
					UNIQUE (ou_id, project_name, agent_name)
			)`
			if err := runSQL(tx, createPublications); err != nil {
				return err
			}

			// The portal catalog lists an org's publications newest update first.
			createCatalogIndex := `
			CREATE INDEX IF NOT EXISTS idx_agent_publications_ou_updated
				ON agent_publications (ou_id, updated_at DESC)`
			return runSQL(tx, createCatalogIndex)
		})
	},
}
