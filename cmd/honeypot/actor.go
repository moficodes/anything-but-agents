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
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	decoyCredentialsFile = "payroll_db_credentials.env.bak"
	decoyCredentialsBody = "DB_HOST=10.99.0.14\nDB_PORT=5432\nDB_USER=payroll_admin\nDB_PASS=Summer2026!Decoy\n"

	decoyWireBatchFile = "wire_transfer_batch_409.csv"
	decoyWireBatchBody = "batch_id,recipient,routing_acct,amount_usd,status\n" +
		"409-A,V. Vance (EVP),021000021-88412,185000.00,PENDING_ADMIN_SIGNOFF\n" +
		"409-B,E. Sterling (CFO),021000021-99104,143500.00,PENDING_ADMIN_SIGNOFF\n" +
		"409-C,Apex Offshore Holdings,026009593-77201,100000.00,PENDING_ADMIN_SIGNOFF\n"
)

type ForensicEvent struct {
	Seq             int       `json:"seq"`
	Timestamp       time.Time `json:"timestamp"`
	Action          string    `json:"action"`
	Command         string    `json:"command,omitempty"`
	Detail          string    `json:"detail"`
	StdoutPreview   string    `json:"stdout_preview,omitempty"`
	ExitCode        int       `json:"exit_code"`
	CanaryTriggered bool      `json:"canary_triggered"`
	PodIP           string    `json:"pod_ip"`
}

type CanaryAlert struct {
	Seq       int       `json:"seq"`
	Timestamp time.Time `json:"timestamp"`
	Severity  string    `json:"severity"`
	Rule      string    `json:"rule"`
	Summary   string    `json:"summary"`
	Command   string    `json:"command,omitempty"`
	PodIP     string    `json:"pod_ip"`
}

type FileDeltaEntry struct {
	Name           string `json:"name"`
	Status         string `json:"status"` // UNCHANGED | ADDED | MODIFIED | DELETED
	SizeBytes      int    `json:"size_bytes"`
	Mode           string `json:"mode"`
	SHA256         string `json:"sha256"`
	ContentPreview string `json:"content_preview"`
}

type SandboxProof struct {
	SandboxClass       string `json:"sandbox_class"`
	KernelVersion      string `json:"kernel_version"`
	RuntimeArch        string `json:"runtime_arch"`
	IsolationMechanism string `json:"isolation_mechanism"`
}

type AttackerShellResponse struct {
	Service      string   `json:"service"`
	Hostname     string   `json:"hostname"`
	DebugMode    bool     `json:"debug_mode"`
	Cwd          string   `json:"cwd"`
	Command      string   `json:"command,omitempty"`
	ExitCode     int      `json:"exit_code"`
	Stdout       string   `json:"stdout"`
	Stderr       string   `json:"stderr,omitempty"`
	FilesInDir   []string `json:"files_in_dir"`
	CurrentPodIP string   `json:"current_pod_ip"`
}

type ForensicsResponse struct {
	Banner             string            `json:"banner"`
	SandboxIsolation   string            `json:"sandbox_isolation"`
	GVisorSandboxProof SandboxProof      `json:"gvisor_sandbox_proof"`
	ScratchDirectory   string            `json:"scratch_directory"`
	RootfsDelta        []FileDeltaEntry  `json:"rootfs_delta"`
	FilesInRootfsTmp   map[string]string `json:"files_in_rootfs_tmp"`
	CanaryAlerts       []CanaryAlert     `json:"canary_alerts"`
	ForensicAuditTrail []ForensicEvent   `json:"forensic_audit_trail"`
	CurrentWorkerPodIP string            `json:"current_worker_pod_ip"`
	PodIPHistory       []string          `json:"pod_ip_history"`
}

type inActorCheckpoint struct {
	Version      int                    `json:"version"`
	Reason       string                 `json:"reason"`
	CreatedAt    time.Time              `json:"created_at"`
	FilesOnDisk  map[string][]byte      `json:"files_on_disk"`
	ModesOnDisk  map[string]os.FileMode `json:"modes_on_disk"`
	KnownFiles   map[string]FileDeltaEntry `json:"known_files"`
	ForensicLog  []ForensicEvent        `json:"forensic_log"`
	CanaryAlerts []CanaryAlert          `json:"canary_alerts"`
}

type Honeypot struct {
	mu           sync.Mutex
	ScratchDir   string
	Baseline     map[string]FileDeltaEntry
	KnownFiles   map[string]FileDeltaEntry
	ForensicLog  []ForensicEvent
	CanaryAlerts []CanaryAlert
	PodIPHistory []string
	CurrentPodIP string
	Checkpoints  map[int]inActorCheckpoint
}

func newHoneypot(scratchDir string) (*Honeypot, error) {
	if err := os.MkdirAll(scratchDir, 0o755); err != nil {
		return nil, err
	}
	seedFiles := map[string]string{
		decoyCredentialsFile: decoyCredentialsBody,
		decoyWireBatchFile:   decoyWireBatchBody,
	}
	for name, body := range seedFiles {
		p := filepath.Join(scratchDir, name)
		if _, err := os.Stat(p); os.IsNotExist(err) {
			if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
				return nil, err
			}
		}
	}

	hp := &Honeypot{
		ScratchDir:   scratchDir,
		Baseline:     make(map[string]FileDeltaEntry),
		KnownFiles:   make(map[string]FileDeltaEntry),
		ForensicLog:  []ForensicEvent{},
		CanaryAlerts: []CanaryAlert{},
		PodIPHistory: []string{},
		Checkpoints:  make(map[int]inActorCheckpoint),
	}
	curr, _ := hp.scanScratchDirLocked()
	for k, v := range curr {
		v.Status = "UNCHANGED"
		hp.Baseline[k] = v
		hp.KnownFiles[k] = v
	}
	hp.saveCheckpointLocked(1, "Golden Snapshot Baseline")
	return hp, nil
}

func (h *Honeypot) saveCheckpointLocked(version int, reason string) inActorCheckpoint {
	filesOnDisk := make(map[string][]byte)
	modesOnDisk := make(map[string]os.FileMode)
	entries, _ := os.ReadDir(h.ScratchDir)
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		full := filepath.Join(h.ScratchDir, entry.Name())
		if data, err := os.ReadFile(full); err == nil {
			filesOnDisk[entry.Name()] = data
		}
		if info, err := entry.Info(); err == nil {
			modesOnDisk[entry.Name()] = info.Mode().Perm()
		}
	}
	knownCopy := make(map[string]FileDeltaEntry, len(h.KnownFiles))
	for k, v := range h.KnownFiles {
		knownCopy[k] = v
	}
	cp := inActorCheckpoint{
		Version:      version,
		Reason:       reason,
		CreatedAt:    time.Now().UTC(),
		FilesOnDisk:  filesOnDisk,
		ModesOnDisk:  modesOnDisk,
		KnownFiles:   knownCopy,
		ForensicLog:  slices.Clone(h.ForensicLog),
		CanaryAlerts: slices.Clone(h.CanaryAlerts),
	}
	if h.Checkpoints == nil {
		h.Checkpoints = make(map[int]inActorCheckpoint)
	}
	h.Checkpoints[version] = cp
	return cp
}

func (h *Honeypot) SaveCheckpoint(version int, reason string) inActorCheckpoint {
	h.mu.Lock()
	defer h.mu.Unlock()
	if version <= 0 {
		version = len(h.Checkpoints) + 1
	}
	return h.saveCheckpointLocked(version, reason)
}

func (h *Honeypot) RestoreCheckpoint(targetVersion int, podIP string) (ForensicsResponse, error) {
	h.mu.Lock()
	defer h.mu.Unlock()

	if targetVersion <= 0 {
		for v := range h.Checkpoints {
			if v > targetVersion {
				targetVersion = v
			}
		}
	}
	cp, ok := h.Checkpoints[targetVersion]
	if !ok {
		// Fall back to highest available checkpoint if requested version wasn't found
		highest := 0
		for v := range h.Checkpoints {
			if v > highest {
				highest = v
			}
		}
		cp, ok = h.Checkpoints[highest]
	}
	if ok {
		entries, _ := os.ReadDir(h.ScratchDir)
		for _, entry := range entries {
			_ = os.RemoveAll(filepath.Join(h.ScratchDir, entry.Name()))
		}
		for name, data := range cp.FilesOnDisk {
			mode := cp.ModesOnDisk[name]
			if mode == 0 {
				mode = 0o644
			}
			_ = os.WriteFile(filepath.Join(h.ScratchDir, name), data, mode)
		}
		h.KnownFiles = make(map[string]FileDeltaEntry, len(cp.KnownFiles))
		for k, v := range cp.KnownFiles {
			h.KnownFiles[k] = v
		}
		h.ForensicLog = slices.Clone(cp.ForensicLog)
		h.CanaryAlerts = slices.Clone(cp.CanaryAlerts)
	}
	_, forensicsResp, err := h.executeInteractionLocked("", "", "", podIP)
	return forensicsResp, err
}

func shortSHA256(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])[:16]
}

func previewString(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}

func (h *Honeypot) recordPodIPLocked(podIP string) {
	if podIP == "" {
		podIP = "127.0.0.1"
	}
	if !strings.HasPrefix(podIP, "169.254.") && podIP != "127.0.0.1" && podIP != "unknown" {
		filtered := make([]string, 0, len(h.PodIPHistory))
		for _, ip := range h.PodIPHistory {
			if !strings.HasPrefix(ip, "169.254.") {
				filtered = append(filtered, ip)
			}
		}
		h.PodIPHistory = filtered
		for i := range h.ForensicLog {
			if strings.HasPrefix(h.ForensicLog[i].PodIP, "169.254.") {
				h.ForensicLog[i].PodIP = podIP
			}
		}
		for i := range h.CanaryAlerts {
			if strings.HasPrefix(h.CanaryAlerts[i].PodIP, "169.254.") {
				h.CanaryAlerts[i].PodIP = podIP
			}
		}
	}
	h.CurrentPodIP = podIP
	if !slices.Contains(h.PodIPHistory, podIP) {
		h.PodIPHistory = append(h.PodIPHistory, podIP)
	}
}

func (h *Honeypot) scanScratchDirLocked() (map[string]FileDeltaEntry, map[string]string) {
	entries, err := os.ReadDir(h.ScratchDir)
	if err != nil {
		return map[string]FileDeltaEntry{}, map[string]string{}
	}
	snap := make(map[string]FileDeltaEntry, len(entries))
	rawMap := make(map[string]string, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		fullPath := filepath.Join(h.ScratchDir, entry.Name())
		b, err := os.ReadFile(fullPath)
		if err != nil {
			continue
		}
		mode := "-rw-r--r--"
		if info, err := entry.Info(); err == nil {
			mode = info.Mode().String()
		}
		rawMap[entry.Name()] = string(b)
		snap[entry.Name()] = FileDeltaEntry{
			Name:           entry.Name(),
			Status:         "UNCHANGED",
			SizeBytes:      len(b),
			Mode:           mode,
			SHA256:         shortSHA256(b),
			ContentPreview: previewString(strings.TrimSpace(string(b)), 160),
		}
	}
	return snap, rawMap
}

func (h *Honeypot) computeDeltaLocked() ([]FileDeltaEntry, map[string]string) {
	curr, rawMap := h.scanScratchDirLocked()
	allNamesMap := make(map[string]struct{})
	for name := range h.KnownFiles {
		allNamesMap[name] = struct{}{}
	}
	for name := range curr {
		allNamesMap[name] = struct{}{}
	}
	names := make([]string, 0, len(allNamesMap))
	for name := range allNamesMap {
		names = append(names, name)
	}
	sort.Strings(names)

	deltas := make([]FileDeltaEntry, 0, len(names))
	for _, name := range names {
		nowEntry, existsNow := curr[name]
		baseEntry, inBaseline := h.Baseline[name]
		knownEntry := h.KnownFiles[name]

		switch {
		case existsNow && inBaseline:
			if nowEntry.SHA256 == baseEntry.SHA256 {
				nowEntry.Status = "UNCHANGED"
			} else {
				nowEntry.Status = "MODIFIED"
			}
			h.KnownFiles[name] = nowEntry
			deltas = append(deltas, nowEntry)
		case existsNow && !inBaseline:
			nowEntry.Status = "ADDED"
			h.KnownFiles[name] = nowEntry
			deltas = append(deltas, nowEntry)
		case !existsNow:
			delEntry := knownEntry
			delEntry.Name = name
			delEntry.Status = "DELETED"
			deltas = append(deltas, delEntry)
		}
	}
	return deltas, rawMap
}

func (h *Honeypot) appendCanaryLocked(severity, rule, summary, cmd, podIP string) {
	h.CanaryAlerts = append(h.CanaryAlerts, CanaryAlert{
		Seq:       len(h.CanaryAlerts) + 1,
		Timestamp: time.Now().UTC(),
		Severity:  severity,
		Rule:      rule,
		Summary:   summary,
		Command:   cmd,
		PodIP:     podIP,
	})
}

func (h *Honeypot) executeInteractionLocked(dropFile, content, cmd, podIP string) (AttackerShellResponse, ForensicsResponse, error) {
	h.recordPodIPLocked(podIP)
	beforeFiles, _ := h.scanScratchDirLocked()

	if dropFile != "" {
		cleanName := filepath.Base(dropFile)
		target := filepath.Join(h.ScratchDir, cleanName)
		perm := os.FileMode(0o644)
		if strings.HasSuffix(cleanName, ".sh") {
			perm = 0o755
		}
		if err := os.WriteFile(target, []byte(content), perm); err != nil {
			return AttackerShellResponse{}, ForensicsResponse{}, fmt.Errorf("write %s: %w", target, err)
		}
		h.appendCanaryLocked(
			"HIGH",
			"ROOTFS_PAYLOAD_PLANTED",
			fmt.Sprintf("Attacker dropped %s (%d bytes) into rootfs overlay", cleanName, len(content)),
			"drop_file="+cleanName,
			podIP,
		)
		h.ForensicLog = append(h.ForensicLog, ForensicEvent{
			Seq:             len(h.ForensicLog) + 1,
			Timestamp:       time.Now().UTC(),
			Action:          "FILE_DROPPED_IN_ROOTFS",
			Command:         "drop_file=" + cleanName,
			Detail:          fmt.Sprintf("wrote %d bytes to %s", len(content), target),
			ExitCode:        0,
			CanaryTriggered: true,
			PodIP:           podIP,
		})
	}

	var stdoutStr, stderrStr string
	exitCode := 0

	if cmd != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()

		execCmd := exec.CommandContext(ctx, "/bin/sh", "-c", cmd)
		execCmd.Dir = h.ScratchDir
		var outBuf, errBuf bytes.Buffer
		execCmd.Stdout = &outBuf
		execCmd.Stderr = &errBuf

		err := execCmd.Run()
		stdoutStr = outBuf.String()
		stderrStr = errBuf.String()
		if err != nil {
			var exitErr *exec.ExitError
			if errors.As(err, &exitErr) {
				exitCode = exitErr.ExitCode()
			} else if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				exitCode = 124
				stderrStr += "\ncommand timed out after 2s"
			} else {
				exitCode = 1
				if stderrStr == "" {
					stderrStr = err.Error()
				}
			}
		}

		canaryFired := false
		// Detect credential or wire-transfer exfiltration
		if (strings.Contains(cmd, decoyCredentialsFile) || strings.Contains(cmd, decoyWireBatchFile) || strings.Contains(stdoutStr, "Summer2026!Decoy")) &&
			!strings.HasPrefix(strings.TrimSpace(cmd), "ls") {
			canaryFired = true
			h.appendCanaryLocked(
				"CRITICAL",
				"DECOY_CREDENTIAL_EXFILTRATION",
				"Attacker read decoy payroll credentials / executive wire batch in /tmp/honeypot",
				cmd,
				podIP,
			)
		}

		// Compare beforeFiles vs current files to detect added/modified/deleted files from shell command
		afterFiles, _ := h.scanScratchDirLocked()
		for name, afterEntry := range afterFiles {
			beforeEntry, existedBefore := beforeFiles[name]
			if !existedBefore {
				canaryFired = true
				h.appendCanaryLocked(
					"HIGH",
					"ROOTFS_PAYLOAD_PLANTED",
					fmt.Sprintf("Shell command created %s (%d bytes, sha256:%s) in rootfs overlay", name, afterEntry.SizeBytes, afterEntry.SHA256),
					cmd,
					podIP,
				)
			} else if beforeEntry.SHA256 != afterEntry.SHA256 {
				canaryFired = true
				h.appendCanaryLocked(
					"HIGH",
					"ROOTFS_FILE_TAMPERED",
					fmt.Sprintf("Shell command modified %s (sha256:%s -> %s)", name, beforeEntry.SHA256, afterEntry.SHA256),
					cmd,
					podIP,
				)
			}
		}
		for name := range beforeFiles {
			if _, stillThere := afterFiles[name]; !stillThere {
				canaryFired = true
				h.appendCanaryLocked(
					"CRITICAL",
					"ANTI_FORENSIC_WIPE_ATTEMPT",
					fmt.Sprintf("Attacker deleted %s from /tmp/honeypot to cover tracks (recoverable via Substrate Revert!)", name),
					cmd,
					podIP,
				)
			}
		}

		h.ForensicLog = append(h.ForensicLog, ForensicEvent{
			Seq:             len(h.ForensicLog) + 1,
			Timestamp:       time.Now().UTC(),
			Action:          "COMMAND_EXECUTED",
			Command:         cmd,
			Detail:          cmd,
			StdoutPreview:   previewString(strings.TrimSpace(stdoutStr), 160),
			ExitCode:        exitCode,
			CanaryTriggered: canaryFired,
			PodIP:           podIP,
		})
	}

	deltas, rawMap := h.computeDeltaLocked()
	fileList := make([]string, 0, len(rawMap))
	for name := range rawMap {
		fileList = append(fileList, name)
	}
	sort.Strings(fileList)

	shellResp := AttackerShellResponse{
		Service:      "AcmeCorp Internal Payroll v1.4 (staging-admin.acmecorp.internal)",
		Hostname:     "payroll-admin-prod-01",
		DebugMode:    true,
		Cwd:          "/var/www/payroll-admin/workspace",
		Command:      cmd,
		ExitCode:     exitCode,
		Stdout:       stdoutStr,
		Stderr:       stderrStr,
		FilesInDir:   fileList,
		CurrentPodIP: h.CurrentPodIP,
	}

	forensicsResp := ForensicsResponse{
		Banner:             "CorpInternal Admin Console v1.4 (gVisor Deception Sandbox)",
		SandboxIsolation:   "SANDBOX_CLASS_GVISOR (rootfs overlay + RAM checkpointed on Suspend)",
		GVisorSandboxProof: detectSandboxProof(),
		ScratchDirectory:   h.ScratchDir,
		RootfsDelta:        deltas,
		FilesInRootfsTmp:   rawMap,
		CanaryAlerts:       slices.Clone(h.CanaryAlerts),
		ForensicAuditTrail: slices.Clone(h.ForensicLog),
		CurrentWorkerPodIP: h.CurrentPodIP,
		PodIPHistory:       slices.Clone(h.PodIPHistory),
	}

	return shellResp, forensicsResp, nil
}

func (h *Honeypot) Interact(dropFile, content, cmd, podIP string) (map[string]any, error) {
	h.mu.Lock()
	defer h.mu.Unlock()

	shellResp, forensicsResp, err := h.executeInteractionLocked(dropFile, content, cmd, podIP)
	if err != nil {
		return nil, err
	}

	return map[string]any{
		"banner":                forensicsResp.Banner,
		"sandbox_isolation":     forensicsResp.SandboxIsolation,
		"gvisor_sandbox_proof":  forensicsResp.GVisorSandboxProof,
		"scratch_directory":     forensicsResp.ScratchDirectory,
		"command":               shellResp.Command,
		"exit_code":             shellResp.ExitCode,
		"stdout":                shellResp.Stdout,
		"stderr":                shellResp.Stderr,
		"rootfs_delta":          forensicsResp.RootfsDelta,
		"files_in_rootfs_tmp":   forensicsResp.FilesInRootfsTmp,
		"canary_alerts":         forensicsResp.CanaryAlerts,
		"forensic_audit_trail":  forensicsResp.ForensicAuditTrail,
		"current_worker_pod_ip": forensicsResp.CurrentWorkerPodIP,
		"pod_ip_history":        forensicsResp.PodIPHistory,
	}, nil
}

func (h *Honeypot) InteractShell(dropFile, content, cmd, podIP string) (AttackerShellResponse, error) {
	h.mu.Lock()
	defer h.mu.Unlock()

	shellResp, _, err := h.executeInteractionLocked(dropFile, content, cmd, podIP)
	return shellResp, err
}

func (h *Honeypot) SnapshotForensics(podIP string) ForensicsResponse {
	h.mu.Lock()
	defer h.mu.Unlock()

	_, forensicsResp, _ := h.executeInteractionLocked("", "", "", podIP)
	return forensicsResp
}

func detectSandboxProof() SandboxProof {
	kernelVer := "Linux 4.4.0 #1 SMP PREEMPT (gVisor Sentry)"
	if b, err := os.ReadFile("/proc/version"); err == nil && len(bytes.TrimSpace(b)) > 0 {
		kernelVer = strings.TrimSpace(string(b))
	}
	return SandboxProof{
		SandboxClass:       "SANDBOX_CLASS_GVISOR",
		KernelVersion:      kernelVer,
		RuntimeArch:        runtime.GOOS + "/" + runtime.GOARCH,
		IsolationMechanism: "gVisor User-Space Kernel (Sentry/Gofer) + SNAPSHOT_CONTENT_SCOPE_FULL (Rootfs Overlay + Heap RAM)",
	}
}

func parseInteractionParams(r *http.Request) (dropFile, content, cmd string) {
	dropFile = r.URL.Query().Get("drop_file")
	content = r.URL.Query().Get("content")
	cmd = r.URL.Query().Get("cmd")

	if r.Method == http.MethodPost && r.Body != nil {
		var body struct {
			DropFile string `json:"drop_file"`
			Content  string `json:"content"`
			Cmd      string `json:"cmd"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err == nil {
			if body.DropFile != "" {
				dropFile = body.DropFile
			}
			if body.Content != "" {
				content = body.Content
			}
			if body.Cmd != "" {
				cmd = body.Cmd
			}
		}
	}
	return dropFile, content, cmd
}

func writePrettyJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(payload)
}

func newMux(hp *Honeypot) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("/shell", func(w http.ResponseWriter, r *http.Request) {
		dropFile, content, cmd := parseInteractionParams(r)
		resp, err := hp.InteractShell(dropFile, content, cmd, resolvePodIP(r))
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writePrettyJSON(w, http.StatusOK, resp)
	})
	mux.HandleFunc("/forensics", func(w http.ResponseWriter, r *http.Request) {
		resp := hp.SnapshotForensics(resolvePodIP(r))
		writePrettyJSON(w, http.StatusOK, resp)
	})
	mux.HandleFunc("/checkpoint/save", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			Version int    `json:"version"`
			Reason  string `json:"reason"`
		}
		if r.Body != nil {
			_ = json.NewDecoder(r.Body).Decode(&req)
		}
		cp := hp.SaveCheckpoint(req.Version, req.Reason)
		writePrettyJSON(w, http.StatusOK, map[string]any{
			"version":    cp.Version,
			"reason":     cp.Reason,
			"created_at": cp.CreatedAt,
		})
	})
	mux.HandleFunc("/checkpoint/restore", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			Version int `json:"version"`
		}
		if r.Body != nil {
			_ = json.NewDecoder(r.Body).Decode(&req)
		}
		resp, err := hp.RestoreCheckpoint(req.Version, resolvePodIP(r))
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writePrettyJSON(w, http.StatusOK, resp)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		dropFile, content, cmd := parseInteractionParams(r)
		resp, err := hp.Interact(dropFile, content, cmd, resolvePodIP(r))
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writePrettyJSON(w, http.StatusOK, resp)
	})
	return mux
}

func resolvePodIP(r *http.Request) string {
	if r != nil {
		if hdr := strings.TrimSpace(r.Header.Get("X-Worker-Pod-IP")); hdr != "" {
			return hdr
		}
		if qp := strings.TrimSpace(r.URL.Query().Get("pod_ip")); qp != "" {
			return qp
		}
	}
	return currentPodIP()
}

func currentPodIP() string {
	if envIP := strings.TrimSpace(os.Getenv("POD_IP")); envIP != "" {
		return envIP
	}
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return "unknown"
	}
	var fallback string
	for _, addr := range addrs {
		if ipnet, ok := addr.(*net.IPNet); ok && !ipnet.IP.IsLoopback() && ipnet.IP.To4() != nil {
			ip := ipnet.IP.String()
			if !ipnet.IP.IsLinkLocalUnicast() && !strings.HasPrefix(ip, "169.254.") {
				return ip
			}
			if fallback == "" {
				fallback = ip
			}
		}
	}
	if fallback != "" {
		return fallback
	}
	return "127.0.0.1"
}
