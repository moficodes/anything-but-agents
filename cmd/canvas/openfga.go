// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"fmt"
	"sort"
	"strings"
	"sync"
)

// OpenFGAEngine provides an embedded OpenFGA relationship authorization engine
// that faithfully evaluates OpenFGA tuples and generates Substrate AccessPolicy YAML.
type OpenFGAEngine struct {
	mu       sync.RWMutex
	atespace string
	tuples   []OpenFGATuple
}

// NewOpenFGAEngine initializes an OpenFGA engine populated with the default demo catalog relationships.
func NewOpenFGAEngine(atespace string) *OpenFGAEngine {
	eng := &OpenFGAEngine{
		atespace: atespace,
	}
	eng.Reset()
	return eng
}

// Reset resets all relationship tuples back to their initial state defined in the catalog.
func (e *OpenFGAEngine) Reset() {
	e.mu.Lock()
	defer e.mu.Unlock()

	e.tuples = nil
	for _, entry := range DefaultDocumentCatalog {
		obj := e.actorObject(entry.DocID)
		if entry.InitialOwner != "" {
			e.tuples = append(e.tuples, OpenFGATuple{
				User:     entry.InitialOwner,
				Relation: RoleOwner,
				Object:   obj,
			})
		}
		if entry.InitialEditor != "" {
			e.tuples = append(e.tuples, OpenFGATuple{
				User:     entry.InitialEditor,
				Relation: RoleEditor,
				Object:   obj,
			})
		}
		if entry.InitialViewer != "" {
			e.tuples = append(e.tuples, OpenFGATuple{
				User:     entry.InitialViewer,
				Relation: RoleViewer,
				Object:   obj,
			})
		}
	}
}

func (e *OpenFGAEngine) actorObject(docID string) string {
	return fmt.Sprintf("actor:%s/%s", e.atespace, docID)
}

// Check evaluates whether user has permission on the given document actor.
// Returns: allowed, effectiveRole, reason.
func (e *OpenFGAEngine) Check(user, permission, docID string) (bool, string, string) {
	e.mu.RLock()
	defer e.mu.RUnlock()

	obj := e.actorObject(docID)
	var activeRole string
	for _, t := range e.tuples {
		if t.User == user && t.Object == obj {
			activeRole = t.Relation
			break
		}
	}

	if activeRole == "" {
		return false, "", fmt.Sprintf("No relationship tuple found for %s on %s", user, obj)
	}

	switch permission {
	case PermCanGet:
		// owner, editor, viewer all have can_get
		if activeRole == RoleOwner || activeRole == RoleEditor || activeRole == RoleViewer {
			return true, activeRole, fmt.Sprintf("%s holds '%s' role with %s permission", user, activeRole, permission)
		}
	case PermCanUpdate, PermCanDelete:
		// owner and editor have can_update / can_delete; viewer does not
		if activeRole == RoleOwner || activeRole == RoleEditor {
			return true, activeRole, fmt.Sprintf("%s holds '%s' role with %s permission", user, activeRole, permission)
		}
		return false, activeRole, fmt.Sprintf("%s holds '%s' role which lacks '%s' permission (read-only)", user, activeRole, permission)
	case PermCanManagePolicy:
		// owner only
		if activeRole == RoleOwner {
			return true, activeRole, fmt.Sprintf("%s holds 'owner' role with %s permission", user, permission)
		}
		return false, activeRole, fmt.Sprintf("%s holds '%s' role which lacks '%s' permission (owner required)", user, activeRole, permission)
	case PermCanResume, PermCanSuspend:
		// owner and editor
		if activeRole == RoleOwner || activeRole == RoleEditor {
			return true, activeRole, fmt.Sprintf("%s holds '%s' role with %s permission", user, activeRole, permission)
		}
		return false, activeRole, fmt.Sprintf("%s holds '%s' role which lacks '%s' permission", user, activeRole, permission)
	}

	return false, activeRole, fmt.Sprintf("Permission '%s' not recognized in OpenFGA model", permission)
}

// GetUserRole returns the direct relation/role the user has on the document actor, or "" if none.
func (e *OpenFGAEngine) GetUserRole(user, docID string) string {
	e.mu.RLock()
	defer e.mu.RUnlock()

	obj := e.actorObject(docID)
	for _, t := range e.tuples {
		if t.User == user && t.Object == obj {
			return t.Relation
		}
	}
	return ""
}

// Grant adds or updates a relationship tuple for user on docID.
func (e *OpenFGAEngine) Grant(user, role, docID string) error {
	if role != RoleOwner && role != RoleEditor && role != RoleViewer {
		return fmt.Errorf("invalid role: %s (must be owner, editor, or viewer)", role)
	}

	e.mu.Lock()
	defer e.mu.Unlock()

	obj := e.actorObject(docID)
	// Remove any existing tuple for (user, obj)
	var filtered []OpenFGATuple
	for _, t := range e.tuples {
		if !(t.User == user && t.Object == obj) {
			filtered = append(filtered, t)
		}
	}
	filtered = append(filtered, OpenFGATuple{
		User:     user,
		Relation: role,
		Object:   obj,
	})
	e.tuples = filtered
	return nil
}

// Revoke removes any relationship tuple for user on docID.
func (e *OpenFGAEngine) Revoke(user, docID string) error {
	e.mu.Lock()
	defer e.mu.Unlock()

	obj := e.actorObject(docID)
	var filtered []OpenFGATuple
	found := false
	for _, t := range e.tuples {
		if t.User == user && t.Object == obj {
			found = true
			continue
		}
		filtered = append(filtered, t)
	}
	if !found {
		return fmt.Errorf("no tuple found for %s on %s", user, obj)
	}
	e.tuples = filtered
	return nil
}

// ListTuples returns all tuples for the given document actor.
func (e *OpenFGAEngine) ListTuples(docID string) []OpenFGATuple {
	e.mu.RLock()
	defer e.mu.RUnlock()

	obj := e.actorObject(docID)
	var res []OpenFGATuple
	for _, t := range e.tuples {
		if t.Object == obj {
			res = append(res, t)
		}
	}
	return res
}

// AllTuples returns a copy of all tuples in the engine.
func (e *OpenFGAEngine) AllTuples() []OpenFGATuple {
	e.mu.RLock()
	defer e.mu.RUnlock()

	res := make([]OpenFGATuple, len(e.tuples))
	copy(res, e.tuples)
	return res
}

// GenerateAccessPolicyYAML formats the current role bindings for docID as an Agent Substrate AccessPolicy manifest.
func (e *OpenFGAEngine) GenerateAccessPolicyYAML(docID string) string {
	tuples := e.ListTuples(docID)

	roleMembers := map[string][]string{
		RoleOwner:  {},
		RoleEditor: {},
		RoleViewer: {},
	}

	for _, t := range tuples {
		roleMembers[t.Relation] = append(roleMembers[t.Relation], t.User)
	}

	for r := range roleMembers {
		sort.Strings(roleMembers[r])
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("# OpenFGA AccessPolicy for %s\n", e.actorObject(docID)))
	sb.WriteString("metadata:\n")
	sb.WriteString(fmt.Sprintf("  name: %s\n", docID))
	sb.WriteString(fmt.Sprintf("  atespace: %s\n", e.atespace))
	sb.WriteString("bindings:\n")

	for _, role := range []string{RoleOwner, RoleEditor, RoleViewer} {
		members := roleMembers[role]
		if len(members) == 0 {
			continue
		}
		sb.WriteString(fmt.Sprintf("- role: %s\n", role))
		sb.WriteString("  members:\n")
		for _, m := range members {
			sb.WriteString(fmt.Sprintf("  - %s\n", m))
		}
	}

	return sb.String()
}
