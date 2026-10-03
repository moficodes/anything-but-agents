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
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"
)

// ActorSession holds 100% of a player's 2D multilevel dungeon game state in
// ordinary Go heap variables. When running under Agent Substrate with
// SNAPSHOT_CONTENT_SCOPE_FULL, suspending the actor checkpoints this entire
// heap struct to GCS, and reverting the actor rewinds it in place.
type ActorSession struct {
	mu             sync.Mutex      `json:"-"`
	BootedAt       time.Time       `json:"booted_at"`
	UpdatedAt      time.Time       `json:"updated_at"`
	HeroName       string          `json:"hero_name"`
	CharacterClass string          `json:"character_class"`
	Floor          int             `json:"floor"`
	X              int             `json:"x"`
	Y              int             `json:"y"`
	HP             int             `json:"hp"`
	MaxHP          int             `json:"max_hp"`
	Gold           int             `json:"gold"`
	Score          int             `json:"score"`
	Alive          bool            `json:"alive"`
	Victory        bool            `json:"victory"`
	DeathReason    string          `json:"death_reason,omitempty"`
	Inventory      []string        `json:"inventory"`
	Cleared        map[string]bool `json:"cleared"`
	MonsterHP      map[string]int  `json:"monster_hp"`
	BossHP         int             `json:"boss_hp"`
	BossMaxHP      int             `json:"boss_max_hp"`
	Turn           int             `json:"turn"`
	MilestoneCount int             `json:"milestone_count"`
	LastEvent      string          `json:"last_event"`
	CurrentPodIP   string          `json:"current_pod_ip"`
	PodIPHistory   []string        `json:"pod_ip_history"`
}

// MutateRequest describes a state milestone sent from the browser via the
// Gateway through atenet-router to wake and mutate the player's actor.
type MutateRequest struct {
	Action         string `json:"action"`
	HeroName       string `json:"hero_name,omitempty"`
	CharacterClass string `json:"character_class,omitempty"`
	Target         string `json:"target,omitempty"`
	EntityID       string `json:"entity_id,omitempty"`
	X              int    `json:"x,omitempty"`
	Y              int    `json:"y,omitempty"`
}

// ActorResponse is the JSON payload returned by the actor after reading or
// mutating its in-memory Go heap state.
type ActorResponse struct {
	Message     string       `json:"message"`
	EventType   string       `json:"event_type"`
	Session     ActorSession `json:"session"`
	Persistence string       `json:"persistence"`
}

func NewActorSession() *ActorSession {
	now := time.Now().UTC()
	return &ActorSession{
		BootedAt:       now,
		UpdatedAt:      now,
		HeroName:       "adventurer",
		CharacterClass: "knight",
		Floor:          1,
		X:              1,
		Y:              1,
		HP:             100,
		MaxHP:          100,
		Gold:           0,
		Score:          0,
		Alive:          true,
		Victory:        false,
		Inventory:      []string{},
		Cleared:        map[string]bool{},
		MonsterHP: map[string]int{
			"f1:skeleton": 30,
			"f2:guard":    45,
			"f3:dragon":   100,
		},
		BossHP:    100,
		BossMaxHP: 100,
		LastEvent: "Entered Floor 1: The Forgotten Crypt (100% Go heap state).",
	}
}

func (s *ActorSession) recordPodIP(ip string) {
	if ip == "" {
		ip = currentPodIP()
	}
	s.CurrentPodIP = ip
	if len(s.PodIPHistory) == 0 || s.PodIPHistory[len(s.PodIPHistory)-1] != ip {
		s.PodIPHistory = append(s.PodIPHistory, ip)
	}
}

func (s *ActorSession) Clone() *ActorSession {
	s.mu.Lock()
	defer s.mu.Unlock()
	raw, _ := json.Marshal(s)
	var out ActorSession
	_ = json.Unmarshal(raw, &out)
	if out.Cleared == nil {
		out.Cleared = map[string]bool{}
	}
	if out.MonsterHP == nil {
		out.MonsterHP = map[string]int{}
	}
	if out.Inventory == nil {
		out.Inventory = []string{}
	}
	return &out
}

func (s *ActorSession) Snapshot(podIP string) ActorResponse {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.recordPodIP(podIP)
	return s.responseLocked(s.LastEvent, "STATE")
}

func (s *ActorSession) Mutate(req MutateRequest, podIP string) ActorResponse {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.recordPodIP(podIP)
	s.UpdatedAt = time.Now().UTC()

	action := strings.ToLower(strings.TrimSpace(req.Action))
	if action == "" || action == "state" || action == "look" {
		return s.responseLocked(s.LastEvent, "STATE")
	}

	if req.X > 0 || req.Y > 0 {
		s.X = req.X
		s.Y = req.Y
	}

	if action == "init" {
		if req.HeroName != "" {
			s.HeroName = req.HeroName
		}
		if req.CharacterClass != "" {
			s.CharacterClass = req.CharacterClass
		}
		if s.X == 0 && s.Y == 0 {
			s.X, s.Y = 1, 1
		}
		msg := fmt.Sprintf("Hero %s (%s) entered Floor 1: The Forgotten Crypt.", s.HeroName, s.CharacterClass)
		s.LastEvent = msg
		return s.responseLocked(msg, "INIT")
	}

	s.Turn++
	s.MilestoneCount++

	if !s.Alive {
		msg := "YOU ARE DEAD! Tap 'Revert Actor' to run `kubectl ate revert` and restore your last GCS heap snapshot."
		return s.responseLocked(msg, "DEAD")
	}
	if s.Victory {
		msg := "VICTORY! You already escaped the Dungeon with the Golden Chalice of State."
		return s.responseLocked(msg, "VICTORY")
	}

	var msg string
	eventType := "MILESTONE"

	switch action {
	case "pickup":
		item := strings.TrimSpace(req.Target)
		entityID := strings.TrimSpace(req.EntityID)
		if entityID == "" {
			entityID = fmt.Sprintf("f%d:%s", s.Floor, item)
		}
		if s.Cleared[entityID] {
			msg = fmt.Sprintf("Already collected %s.", item)
		} else {
			s.Cleared[entityID] = true
			if item == "gold-chest" {
				s.Gold += 100
				s.Score += 150
				msg = "Looted 100 Gold from the ancient chest! Checkpointed to Go heap."
			} else {
				if !slices.Contains(s.Inventory, item) || item == "healing-potion" {
					s.Inventory = append(s.Inventory, item)
				}
				s.Score += 100
				msg = fmt.Sprintf("Picked up %s on Floor %d! Stored in Go heap slice and committed to GCS.", item, s.Floor)
			}
		}
		eventType = "PICKUP"

	case "drink":
		idx := slices.Index(s.Inventory, "healing-potion")
		if idx == -1 {
			msg = "No healing-potion in inventory!"
		} else {
			s.Inventory = slices.Delete(s.Inventory, idx, idx+1)
			s.HP = min(s.MaxHP, s.HP+50)
			s.Score += 25
			msg = fmt.Sprintf("Drank healing-potion! HP restored to %d/%d.", s.HP, s.MaxHP)
		}
		eventType = "HEAL"

	case "combat":
		entityID := strings.TrimSpace(req.EntityID)
		target := strings.TrimSpace(req.Target)
		if entityID == "" {
			switch s.Floor {
			case 1:
				entityID = "f1:skeleton"
			case 2:
				entityID = "f2:guard"
			default:
				entityID = "f3:dragon"
			}
		}
		if target == "" {
			target = entityID
		}
		if s.Cleared[entityID] {
			msg = fmt.Sprintf("%s is already defeated!", target)
			eventType = "COMBAT"
			break
		}

		playerDmg := 30
		if slices.Contains(s.Inventory, "dragon-slayer-sword") {
			playerDmg = 55
		}

		switch entityID {
		case "f1:skeleton":
			s.MonsterHP[entityID] = 0
			s.Cleared[entityID] = true
			s.HP = max(1, s.HP-10)
			s.Gold += 50
			s.Score += 200
			msg = fmt.Sprintf("Shattered %s! (-10 HP, +200 Score). The path to the Crypt Key is clear.", target)
			eventType = "COMBAT_WIN"

		case "f2:guard":
			s.MonsterHP[entityID] = 0
			s.Cleared[entityID] = true
			s.HP = max(1, s.HP-15)
			s.Gold += 75
			s.Score += 250
			msg = fmt.Sprintf("Vanquished %s! (-15 HP, +250 Score). The Fire Shield vault is unlocked.", target)
			eventType = "COMBAT_WIN"

		case "f3:dragon":
			curHP := s.BossHP
			if curHP <= 0 {
				curHP = 100
			}
			curHP = max(0, curHP-playerDmg)
			s.BossHP = curHP
			s.MonsterHP[entityID] = curHP

			if !slices.Contains(s.Inventory, "fire-shield") {
				s.HP = 0
				s.Alive = false
				s.DeathReason = "Incinerated by unbuffered Heap Dragon fire breath (missing fire-shield)!"
				msg = fmt.Sprintf("You struck %s for %d damage, but without the fire-shield its unbuffered fire breath incinerates your heap! YOU DIED (HP=0). Tap REVERT ACTOR to time-travel back to your last GCS snapshot!", target, playerDmg)
				eventType = "DEATH"
			} else if curHP == 0 {
				s.Cleared[entityID] = true
				s.Gold += 500
				s.Score += 1000
				msg = fmt.Sprintf("SLAYED THE %s! Your fire-shield absorbed the flames and your dragon-slayer-sword shattered its scales! Claim the Golden Chalice of State!", strings.ToUpper(target))
				eventType = "BOSS_DEFEATED"
			} else {
				counter := 15
				s.HP = max(0, s.HP-counter)
				if s.HP == 0 {
					s.Alive = false
					s.DeathReason = "Fell in battle against the Ancient Heap Dragon."
					msg = fmt.Sprintf("You struck %s (%d HP left), but its tail swipe finished you off! YOU DIED.", target, curHP)
					eventType = "DEATH"
				} else {
					s.Score += 150
					msg = fmt.Sprintf("Struck %s for %d DMG (%d/%d Boss HP left)! Your fire-shield deflected its lethal breath (-%d HP; Your HP: %d).", target, playerDmg, curHP, s.BossMaxHP, counter, s.HP)
					eventType = "COMBAT"
				}
			}

		default:
			s.Cleared[entityID] = true
			s.Score += 100
			msg = fmt.Sprintf("Defeated %s!", target)
			eventType = "COMBAT_WIN"
		}

	case "quicksave":
		s.Cleared["f2:shrine"] = true
		s.Score += 50
		msg = fmt.Sprintf("QUICKSAVE SHRINE ACTIVATED on Floor %d! Full Go heap checkpointed to GCS and tagged.", s.Floor)
		eventType = "QUICKSAVE"

	case "stairs":
		if s.Floor == 1 {
			if !slices.Contains(s.Inventory, "crypt-key") {
				msg = "The Crypt stairs are locked! Collect the crypt-key on Floor 1 first."
				eventType = "BLOCKED"
			} else {
				s.Floor = 2
				s.X, s.Y = 1, 5
				s.Score += 200
				msg = "Descended to Floor 2: The Abandoned Armory! Tip: grab the dragon-slayer-sword, unlock the northern Fire Shield vault, and touch the Quicksave Shrine."
				eventType = "FLOOR_CHANGE"
			}
		} else if s.Floor == 2 {
			s.Floor = 3
			s.X, s.Y = 7, 9
			s.Score += 300
			msg = "Descended to Floor 3: Cavern of the Ancient Heap Dragon!"
			eventType = "FLOOR_CHANGE"
		} else {
			msg = "Already on the deepest floor (Floor 3)."
		}

	case "escape":
		if !slices.Contains(s.Inventory, "golden-chalice") {
			msg = "The Ascension Portal requires the golden-chalice! Defeat the Ancient Heap Dragon first."
			eventType = "BLOCKED"
		} else {
			s.Victory = true
			s.Score += 1500 + (s.HP * 5)
			msg = fmt.Sprintf("VICTORY! %s escaped the 3-Floor Dungeon with the Golden Chalice of State (Final Score: %d, Pod IPs visited: %d)!", s.HeroName, s.Score, len(s.PodIPHistory))
			eventType = "VICTORY"
		}

	default:
		msg = fmt.Sprintf("Unknown milestone action %q.", action)
	}

	s.LastEvent = msg
	return s.responseLocked(msg, eventType)
}

func (s *ActorSession) responseLocked(msg, eventType string) ActorResponse {
	inv := make([]string, len(s.Inventory))
	copy(inv, s.Inventory)
	cleared := make(map[string]bool, len(s.Cleared))
	for k, v := range s.Cleared {
		cleared[k] = v
	}
	mhp := make(map[string]int, len(s.MonsterHP))
	for k, v := range s.MonsterHP {
		mhp[k] = v
	}
	ips := make([]string, len(s.PodIPHistory))
	copy(ips, s.PodIPHistory)

	snap := ActorSession{
		BootedAt:       s.BootedAt,
		UpdatedAt:      s.UpdatedAt,
		HeroName:       s.HeroName,
		CharacterClass: s.CharacterClass,
		Floor:          s.Floor,
		X:              s.X,
		Y:              s.Y,
		HP:             s.HP,
		MaxHP:          s.MaxHP,
		Gold:           s.Gold,
		Score:          s.Score,
		Alive:          s.Alive,
		Victory:        s.Victory,
		DeathReason:    s.DeathReason,
		Inventory:      inv,
		Cleared:        cleared,
		MonsterHP:      mhp,
		BossHP:         s.BossHP,
		BossMaxHP:      s.BossMaxHP,
		Turn:           s.Turn,
		MilestoneCount: s.MilestoneCount,
		LastEvent:      s.LastEvent,
		CurrentPodIP:   s.CurrentPodIP,
		PodIPHistory:   ips,
	}
	return ActorResponse{
		Message:     msg,
		EventType:   eventType,
		Session:     snap,
		Persistence: "ZERO_DATABASE_PURE_PROCESS_RAM_GCS_CHECKPOINT",
	}
}

func NewActorMux(session *ActorSession) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("/state", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, session.Snapshot(currentPodIP()))
	})
	mux.HandleFunc("/mutate", func(w http.ResponseWriter, r *http.Request) {
		var req MutateRequest
		if r.Body != nil {
			body, _ := io.ReadAll(io.LimitReader(r.Body, 16384))
			_ = json.Unmarshal(body, &req)
		}
		if req.Action == "" {
			req.Action = r.URL.Query().Get("action")
		}
		if req.Target == "" {
			req.Target = r.URL.Query().Get("target")
		}
		if req.EntityID == "" {
			req.EntityID = r.URL.Query().Get("entity_id")
		}
		if req.HeroName == "" {
			req.HeroName = r.URL.Query().Get("hero")
		}
		writeJSON(w, http.StatusOK, session.Mutate(req, currentPodIP()))
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost || r.URL.Query().Get("action") != "" {
			var req MutateRequest
			if r.Body != nil {
				body, _ := io.ReadAll(io.LimitReader(r.Body, 16384))
				_ = json.Unmarshal(body, &req)
			}
			if req.Action == "" {
				req.Action = r.URL.Query().Get("action")
			}
			if req.Target == "" {
				req.Target = r.URL.Query().Get("target")
			}
			if req.EntityID == "" {
				req.EntityID = r.URL.Query().Get("entity_id")
			}
			if req.HeroName == "" {
				req.HeroName = r.URL.Query().Get("hero")
			}
			writeJSON(w, http.StatusOK, session.Mutate(req, currentPodIP()))
			return
		}
		writeJSON(w, http.StatusOK, session.Snapshot(currentPodIP()))
	})
	return mux
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(payload)
}

func currentPodIP() string {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return "unknown"
	}
	for _, addr := range addrs {
		if ipnet, ok := addr.(*net.IPNet); ok && !ipnet.IP.IsLoopback() && ipnet.IP.To4() != nil {
			return ipnet.IP.String()
		}
	}
	return "127.0.0.1"
}
