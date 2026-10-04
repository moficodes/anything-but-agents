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

// Command honeypot is a "Truman Show" deception sandbox.
package main

import (
	"flag"
	"log/slog"
	"net/http"
	"os"
)

func main() {
	addr := flag.String("addr", ":80", "HTTP listen address")
	scratchDir := flag.String("scratch-dir", "/tmp/honeypot", "Rootfs directory for attacker-dropped files")
	flag.Parse()

	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))
	hp, err := newHoneypot(*scratchDir)
	if err != nil {
		slog.Error("Failed to initialize honeypot", slog.Any("err", err))
		os.Exit(1)
	}
	slog.Info("Starting Truman Show Honeypot server", slog.String("addr", *addr), slog.String("scratch_dir", *scratchDir))
	if err := http.ListenAndServe(*addr, newMux(hp)); err != nil {
		slog.Error("Server exited", slog.Any("err", err))
		os.Exit(1)
	}
}
