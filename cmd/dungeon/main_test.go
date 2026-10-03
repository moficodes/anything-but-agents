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

func TestDungeonGameplayAndPodHistory(t *testing.T) {
	g := newGame()

	// Turn 1 on worker pod 10.0.0.1: move to dragon-lair via armory without picking up fire-shield.
	g.Execute("move armory", "10.0.0.1")
	g.Execute("take dragon-slayer-sword", "10.0.0.1")
	g.Execute("move dragon-lair", "10.0.0.2") // Simulate resume on a new worker pod!

	if len(g.PodIPHistory) != 2 || g.PodIPHistory[0] != "10.0.0.1" || g.PodIPHistory[1] != "10.0.0.2" {
		t.Fatalf("expected pod IP migration history [10.0.0.1 10.0.0.2], got %v", g.PodIPHistory)
	}

	// Attacking the dragon without fire-shield should kill the player (setting up `kubectl ate revert`).
	g.Execute("attack", "10.0.0.2")
	if g.Alive || g.PlayerHP != 0 {
		t.Fatalf("expected player to die when attacking dragon without fire-shield, got alive=%v hp=%d", g.Alive, g.PlayerHP)
	}
}

func TestDungeonVictoryWithShieldAndSword(t *testing.T) {
	g := newGame()
	g.Execute("move armory", "10.0.0.1")
	g.Execute("take dragon-slayer-sword", "10.0.0.1")
	g.Execute("take fire-shield", "10.0.0.1")
	g.Execute("move dragon-lair", "10.0.0.1")
	g.Execute("attack", "10.0.0.1")
	g.Execute("attack", "10.0.0.1")

	if !g.Alive {
		t.Fatalf("expected player with fire-shield to survive dragon fight")
	}
	if g.Rooms["dragon-lair"].MonsterHP != 0 {
		t.Fatalf("expected dragon to be defeated in 2 hits with sword, got HP=%d", g.Rooms["dragon-lair"].MonsterHP)
	}
}
