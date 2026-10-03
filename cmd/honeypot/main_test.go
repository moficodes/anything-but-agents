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
	"testing"
)

func TestHoneypotRootfsDropAndForensicTrail(t *testing.T) {
	hp, err := newHoneypot(t.TempDir())
	if err != nil {
		t.Fatalf("newHoneypot: %v", err)
	}

	resp, err := hp.Interact("backdoor.sh", "#!/bin/sh\necho pwned", "whoami", "10.0.0.12")
	if err != nil {
		t.Fatalf("Interact: %v", err)
	}

	files := resp["files_in_rootfs_tmp"].(map[string]string)
	if files["backdoor.sh"] != "#!/bin/sh\necho pwned" {
		t.Fatalf("expected backdoor.sh in rootfs tmp, got %v", files)
	}
	if _, ok := files["payroll_db_credentials.env.bak"]; !ok {
		t.Fatalf("expected decoy credentials file to exist")
	}

	events := resp["forensic_audit_trail"].([]ForensicEvent)
	if len(events) != 2 {
		t.Fatalf("expected 2 forensic events, got %d", len(events))
	}
}
