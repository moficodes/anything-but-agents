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

// Command dungeon is a Zork-style text adventure where 100% of game state
// (player HP, inventory, monster HP, dropped items on the floor, and turn log)
// lives in ordinary Go heap variables. There is no database, no file I/O, and
// no state serialization code — Agent Substrate's Full memory snapshots turn
// process RAM itself into durable storage across suspend, resume, revert, and
// tag-cloning.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"slices"
	"strings"
	"sync"
	"time"
)

type Room struct {
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Items       []string `json:"items_on_floor"`
	Monster     string   `json:"monster,omitempty"`
	MonsterHP   int      `json:"monster_hp,omitempty"`
	Exits       []string `json:"exits"`
}

type GameState struct {
	mu           sync.Mutex
	BootedAt     time.Time        `json:"booted_at"`
	Turn         int              `json:"turn"`
	PlayerHP     int              `json:"player_hp"`
	Alive        bool             `json:"alive"`
	CurrentRoom  string           `json:"current_room"`
	Inventory    []string         `json:"inventory"`
	Rooms        map[string]*Room `json:"rooms"`
	LastAction   string           `json:"last_action"`
	CurrentPodIP string           `json:"current_pod_ip"`
	PodIPHistory []string         `json:"pod_ip_history"`
}

func newGame() *GameState {
	return &GameState{
		BootedAt:    time.Now().UTC(),
		Turn:        0,
		PlayerHP:    100,
		Alive:       true,
		CurrentRoom: "tavern",
		Inventory:   []string{"lantern"},
		Rooms: map[string]*Room{
			"tavern": {
				Name:        "The Sleeping Gopher Tavern",
				Description: "A cozy tavern smelling of warm RAM and freshly checkpointed pages.",
				Items:       []string{"healing-potion"},
				Exits:       []string{"armory", "crypt"},
			},
			"armory": {
				Name:        "Abandoned Armory",
				Description: "Weapon racks line the stone walls.",
				Items:       []string{"dragon-slayer-sword", "fire-shield"},
				Exits:       []string{"tavern", "dragon-lair"},
			},
			"crypt": {
				Name:        "Forgotten Crypt",
				Description: "Cobwebs hang from low arches. Something rattles in the dark.",
				Items:       []string{"ancient-key"},
				Monster:     "Restless Skeleton",
				MonsterHP:   30,
				Exits:       []string{"tavern", "dragon-lair"},
			},
			"dragon-lair": {
				Name:        "Cavern of the Heap Dragon",
				Description: "A massive cavern piled high with uncollected garbage and golden snapshots.",
				Items:       []string{"golden-chalice"},
				Monster:     "Ancient Heap Dragon",
				MonsterHP:   100,
				Exits:       []string{"armory", "crypt"},
			},
		},
		LastAction: "Entered the dungeon (state lives 100% in Go heap variables — zero DB, zero disk writes).",
	}
}

func (g *GameState) recordPodIP(ip string) {
	g.CurrentPodIP = ip
	if len(g.PodIPHistory) == 0 || g.PodIPHistory[len(g.PodIPHistory)-1] != ip {
		g.PodIPHistory = append(g.PodIPHistory, ip)
	}
}

func (g *GameState) Execute(cmdLine string, podIP string) map[string]any {
	g.mu.Lock()
	defer g.mu.Unlock()

	g.recordPodIP(podIP)
	cmdLine = strings.TrimSpace(cmdLine)
	if cmdLine == "" || cmdLine == "look" {
		return g.snapshotLocked("You look around.")
	}

	g.Turn++
	if !g.Alive {
		return g.snapshotLocked("YOU ARE DEAD! Hint: run `kubectl ate revert actor <name> -a <atespace>` to time-travel back to your last suspended snapshot!")
	}

	parts := strings.Fields(strings.ToLower(cmdLine))
	verb := parts[0]
	arg := ""
	if len(parts) > 1 {
		arg = parts[1]
	}

	room := g.Rooms[g.CurrentRoom]
	var msg string

	switch verb {
	case "move", "go":
		if !slices.Contains(room.Exits, arg) {
			msg = fmt.Sprintf("Cannot move to %q from %s. Available exits: %v", arg, g.CurrentRoom, room.Exits)
		} else {
			g.CurrentRoom = arg
			nextRoom := g.Rooms[g.CurrentRoom]
			msg = fmt.Sprintf("Moved to %s: %s", nextRoom.Name, nextRoom.Description)
		}

	case "take", "get":
		idx := slices.Index(room.Items, arg)
		if idx == -1 {
			msg = fmt.Sprintf("No %q on the floor here. Floor items: %v", arg, room.Items)
		} else {
			room.Items = slices.Delete(room.Items, idx, idx+1)
			g.Inventory = append(g.Inventory, arg)
			msg = fmt.Sprintf("Picked up %s (stored purely in Go slice in process RAM).", arg)
		}

	case "drop":
		idx := slices.Index(g.Inventory, arg)
		if idx == -1 {
			msg = fmt.Sprintf("You don't have %q in your inventory.", arg)
		} else {
			g.Inventory = slices.Delete(g.Inventory, idx, idx+1)
			room.Items = append(room.Items, arg)
			msg = fmt.Sprintf("Dropped %s onto the floor of %s.", arg, room.Name)
		}

	case "drink", "use":
		idx := slices.Index(g.Inventory, "healing-potion")
		if arg != "healing-potion" || idx == -1 {
			msg = "You need a healing-potion in your inventory to drink."
		} else {
			g.Inventory = slices.Delete(g.Inventory, idx, idx+1)
			g.PlayerHP = min(100, g.PlayerHP+50)
			msg = fmt.Sprintf("Drank healing-potion! HP restored to %d.", g.PlayerHP)
		}

	case "attack", "fight":
		if room.Monster == "" || room.MonsterHP <= 0 {
			msg = "There is nothing to attack here."
		} else {
			damage := 15
			if slices.Contains(g.Inventory, "dragon-slayer-sword") {
				damage = 55
			}
			room.MonsterHP = max(0, room.MonsterHP-damage)
			if room.MonsterHP == 0 {
				msg = fmt.Sprintf("You strike %s for %d damage and DEFEAT it!", room.Monster, damage)
			} else if room.Monster == "Ancient Heap Dragon" && !slices.Contains(g.Inventory, "fire-shield") {
				g.PlayerHP = 0
				g.Alive = false
				msg = fmt.Sprintf("You hit %s for %d damage, but without a fire-shield its unbuffered fire breath incinerates your heap! YOU DIED (HP=0). Use `kubectl ate revert actor` to restore from your last snapshot!", room.Monster, damage)
			} else {
				counterDmg := 10
				g.PlayerHP = max(0, g.PlayerHP-counterDmg)
				if g.PlayerHP == 0 {
					g.Alive = false
					msg = fmt.Sprintf("You hit %s for %d damage, but its counterattack finishes you off! YOU DIED.", room.Monster, damage)
				} else {
					msg = fmt.Sprintf("You hit %s for %d damage (%d HP left). It hits you back for %d damage (Your HP: %d).", room.Monster, damage, room.MonsterHP, counterDmg, g.PlayerHP)
				}
			}
		}

	default:
		msg = fmt.Sprintf("Unknown command %q. Try: look, move <exit>, take <item>, drop <item>, drink healing-potion, attack", verb)
	}

	g.LastAction = msg
	return g.snapshotLocked(msg)
}

func (g *GameState) snapshotLocked(message string) map[string]any {
	room := g.Rooms[g.CurrentRoom]
	return map[string]any{
		"message":        message,
		"turn":           g.Turn,
		"alive":          g.Alive,
		"player_hp":      g.PlayerHP,
		"inventory":      g.Inventory,
		"current_room":   g.CurrentRoom,
		"room_details":   room,
		"current_pod_ip": g.CurrentPodIP,
		"pod_ip_history": g.PodIPHistory,
		"persistence":    "ZERO_DATABASE_PURE_PROCESS_RAM",
	}
}

func newMux(game *GameState) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		cmdLine := r.URL.Query().Get("cmd")
		if cmdLine == "" && r.Method == http.MethodPost {
			body, _ := io.ReadAll(io.LimitReader(r.Body, 4096))
			var payload struct {
				Cmd string `json:"cmd"`
			}
			if json.Unmarshal(body, &payload) == nil && payload.Cmd != "" {
				cmdLine = payload.Cmd
			} else {
				cmdLine = strings.TrimSpace(string(body))
			}
		}
		resp := game.Execute(cmdLine, currentPodIP())
		w.Header().Set("Content-Type", "application/json")
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		_ = enc.Encode(resp)
	})
	return mux
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

func main() {
	addr := flag.String("addr", ":80", "HTTP listen address")
	flag.Parse()

	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))
	game := newGame()
	slog.Info("Starting 'Look Ma, No Database' Dungeon server", slog.String("addr", *addr))
	if err := http.ListenAndServe(*addr, newMux(game)); err != nil {
		slog.Error("Server exited", slog.Any("err", err))
		os.Exit(1)
	}
}
