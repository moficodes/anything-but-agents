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

// Command dungeon2d provides both the per-player Agent Substrate actor server
// (--mode=actor) and the 2D browser game + Live Mission-Control Dashboard
// gateway (--mode=gateway or --mode=local).
package main

import (
	"flag"
	"log/slog"
	"net/http"
	"os"
)

func main() {
	addr := flag.String("addr", ":80", "HTTP listen address")
	mode := flag.String("mode", "actor", "Operating mode: actor | gateway | local")
	flag.Parse()

	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))
	if *mode == "actor" {
		session := NewActorSession()
		slog.Info("Starting dungeon2d per-player actor server", slog.String("addr", *addr))
		if err := http.ListenAndServe(*addr, NewActorMux(session)); err != nil {
			slog.Error("Server exited", slog.Any("err", err))
			os.Exit(1)
		}
	}
}
