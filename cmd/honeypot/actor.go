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
	"io"
	"io/fs"
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
	decoySecretURI       = "ate-secret://k8s.io/default/anything-but-agents/payroll-vault-api/token"
	decoyTrustBundlePath = "/run/ate/trust-bundle.pem"
	decoyCredentialsBody = "DB_HOST=10.99.0.14\nDB_PORT=5432\nDB_USER=payroll_admin\nDB_PASS=Summer2026!Decoy\nPAYROLL_API_TOKEN=" + decoySecretURI + "\n"

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

type EgressSecurityProof struct {
	PolicyName             string   `json:"policy_name"`
	DefaultAction          string   `json:"default_action"`
	AllowedHosts           []string `json:"allowed_hosts"`
	CredentialInjectionURI string   `json:"credential_injection_uri"`
	TrustBundlePath        string   `json:"trust_bundle_path"`
	TrustBundlePresent     bool     `json:"trust_bundle_present"`
	BlockedEgressAttempts  int      `json:"blocked_egress_attempts"`
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
	Banner              string              `json:"banner"`
	SandboxIsolation    string              `json:"sandbox_isolation"`
	GVisorSandboxProof  SandboxProof        `json:"gvisor_sandbox_proof"`
	EgressSecurityProof EgressSecurityProof `json:"egress_security_proof"`
	ScratchDirectory    string              `json:"scratch_directory"`
	CurrentWorkingDir   string              `json:"current_working_dir"`
	RootfsDelta         []FileDeltaEntry    `json:"rootfs_delta"`
	FilesInRootfsTmp    map[string]string   `json:"files_in_rootfs_tmp"`
	CanaryAlerts        []CanaryAlert       `json:"canary_alerts"`
	ForensicAuditTrail  []ForensicEvent     `json:"forensic_audit_trail"`
	CurrentWorkerPodIP  string              `json:"current_worker_pod_ip"`
	PodIPHistory        []string            `json:"pod_ip_history"`
}

type inActorCheckpoint struct {
	Version       int                       `json:"version"`
	Reason        string                    `json:"reason"`
	CreatedAt     time.Time                 `json:"created_at"`
	DirsOnDisk    map[string]os.FileMode    `json:"dirs_on_disk"`
	FilesOnDisk   map[string][]byte         `json:"files_on_disk"`
	ModesOnDisk   map[string]os.FileMode    `json:"modes_on_disk"`
	KnownFiles    map[string]FileDeltaEntry `json:"known_files"`
	CurrentDir    string                    `json:"current_dir"`
	OldDir        string                    `json:"old_dir"`
	ShellEnv      map[string]string         `json:"shell_env"`
	ForensicLog   []ForensicEvent           `json:"forensic_log"`
	CanaryAlerts  []CanaryAlert             `json:"canary_alerts"`
	BlockedEgress int                       `json:"blocked_egress"`
}

type Honeypot struct {
	mu            sync.Mutex
	ScratchDir    string
	CurrentDir    string
	OldDir        string
	ShellEnv      map[string]string
	Baseline      map[string]FileDeltaEntry
	KnownFiles    map[string]FileDeltaEntry
	ForensicLog   []ForensicEvent
	CanaryAlerts  []CanaryAlert
	BlockedEgress int
	PodIPHistory  []string
	CurrentPodIP  string
	Checkpoints   map[int]inActorCheckpoint
	OnCrash       func(code int)
}

func newHoneypot(scratchDir string) (*Honeypot, error) {
	if err := os.MkdirAll(scratchDir, 0o755); err != nil {
		return nil, err
	}
	if resolved, err := filepath.EvalSymlinks(scratchDir); err == nil && resolved != "" {
		scratchDir = resolved
	} else if abs, err := filepath.Abs(scratchDir); err == nil && abs != "" {
		scratchDir = abs
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

	pathEnv := os.Getenv("PATH")
	if pathEnv == "" {
		pathEnv = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
	}
	homeEnv := "/root"
	if scratchDir != "/tmp/honeypot" {
		homeEnv = scratchDir
	} else if envHome := os.Getenv("HOME"); envHome != "" {
		homeEnv = envHome
	}

	hp := &Honeypot{
		ScratchDir: scratchDir,
		CurrentDir: scratchDir,
		OldDir:     scratchDir,
		ShellEnv: map[string]string{
			"PATH":              pathEnv,
			"HOME":              homeEnv,
			"USER":              "root",
			"LOGNAME":           "root",
			"SHELL":             "/bin/sh",
			"TERM":              "xterm-256color",
			"HOSTNAME":          "payroll-admin-prod-01",
			"LANG":              "C.UTF-8",
			"SSL_CERT_FILE":     decoyTrustBundlePath,
			"SSL_CERT_DIR":      "/run/ate",
			"PAYROLL_API_TOKEN": decoySecretURI,
		},
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

func cloneStringMap(m map[string]string) map[string]string {
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func (h *Honeypot) saveCheckpointLocked(version int, reason string) inActorCheckpoint {
	dirsOnDisk := make(map[string]os.FileMode)
	filesOnDisk := make(map[string][]byte)
	modesOnDisk := make(map[string]os.FileMode)
	_ = filepath.WalkDir(h.ScratchDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || path == h.ScratchDir {
			return nil
		}
		if d.Type()&os.ModeSymlink != 0 {
			return nil
		}
		rel, relErr := filepath.Rel(h.ScratchDir, path)
		if relErr != nil || rel == "." {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			perm := os.FileMode(0o755)
			if info, infoErr := d.Info(); infoErr == nil {
				perm = info.Mode().Perm()
			}
			dirsOnDisk[rel] = perm
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		if data, readErr := os.ReadFile(path); readErr == nil {
			filesOnDisk[rel] = data
		}
		if info, infoErr := d.Info(); infoErr == nil {
			modesOnDisk[rel] = info.Mode().Perm()
		}
		return nil
	})
	knownCopy := make(map[string]FileDeltaEntry, len(h.KnownFiles))
	for k, v := range h.KnownFiles {
		knownCopy[k] = v
	}
	cp := inActorCheckpoint{
		Version:       version,
		Reason:        reason,
		CreatedAt:     time.Now().UTC(),
		DirsOnDisk:    dirsOnDisk,
		FilesOnDisk:   filesOnDisk,
		ModesOnDisk:   modesOnDisk,
		KnownFiles:    knownCopy,
		CurrentDir:    h.CurrentDir,
		OldDir:        h.OldDir,
		ShellEnv:      cloneStringMap(h.ShellEnv),
		ForensicLog:   slices.Clone(h.ForensicLog),
		CanaryAlerts:  slices.Clone(h.CanaryAlerts),
		BlockedEgress: h.BlockedEgress,
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
		dirNames := make([]string, 0, len(cp.DirsOnDisk))
		for d := range cp.DirsOnDisk {
			dirNames = append(dirNames, d)
		}
		sort.Strings(dirNames)
		for _, dirRel := range dirNames {
			cleanRel := filepath.Clean(filepath.FromSlash(dirRel))
			if cleanRel == "." || strings.HasPrefix(cleanRel, "..") || filepath.IsAbs(cleanRel) {
				continue
			}
			perm := cp.DirsOnDisk[dirRel]
			if perm == 0 {
				perm = 0o755
			}
			_ = os.MkdirAll(filepath.Join(h.ScratchDir, cleanRel), perm)
		}
		for name, data := range cp.FilesOnDisk {
			cleanRel := filepath.Clean(filepath.FromSlash(name))
			if cleanRel == "." || strings.HasPrefix(cleanRel, "..") || filepath.IsAbs(cleanRel) {
				continue
			}
			mode := cp.ModesOnDisk[name]
			if mode == 0 {
				mode = 0o644
			}
			target := filepath.Join(h.ScratchDir, cleanRel)
			_ = os.MkdirAll(filepath.Dir(target), 0o755)
			_ = os.WriteFile(target, data, mode)
		}
		h.KnownFiles = make(map[string]FileDeltaEntry, len(cp.KnownFiles))
		for k, v := range cp.KnownFiles {
			h.KnownFiles[k] = v
		}
		if cp.CurrentDir != "" {
			if info, err := os.Stat(cp.CurrentDir); err == nil && info.IsDir() {
				h.CurrentDir = cp.CurrentDir
			} else {
				h.CurrentDir = h.ScratchDir
			}
		} else {
			h.CurrentDir = h.ScratchDir
		}
		if cp.OldDir != "" {
			h.OldDir = cp.OldDir
		} else {
			h.OldDir = h.ScratchDir
		}
		if len(cp.ShellEnv) > 0 {
			h.ShellEnv = cloneStringMap(cp.ShellEnv)
		}
		h.ForensicLog = slices.Clone(cp.ForensicLog)
		h.CanaryAlerts = slices.Clone(cp.CanaryAlerts)
		h.BlockedEgress = cp.BlockedEgress
	}
	_, forensicsResp, err := h.executeInteractionLocked("", "", "", podIP)
	return forensicsResp, err
}

func isCrashCommand(cmd string) bool {
	trimmed := strings.TrimSpace(cmd)
	switch trimmed {
	case "kill -9 1", "kill -KILL 1", "kill -s KILL 1", "kill -SIGKILL 1", "crash-pid1":
		return true
	default:
		return false
	}
}

func detectOutboundC2Attempt(cmd string) (bool, string) {
	// Split on shell command separators (&&, ||, ;) outside quotes to inspect actual invoked commands
	segments := strings.FieldsFunc(cmd, func(r rune) bool {
		return r == ';' || r == '&' || r == '|'
	})
	hasNetTool := false
	for _, seg := range segments {
		fields := strings.Fields(strings.TrimSpace(seg))
		if len(fields) == 0 {
			continue
		}
		first := strings.ToLower(fields[0])
		if first == "curl" || first == "wget" || first == "nc" || first == "ncat" {
			hasNetTool = true
			for _, token := range fields[1:] {
				cleaned := strings.Trim(token, "'\"`()")
				if strings.HasPrefix(cleaned, "http://") || strings.HasPrefix(cleaned, "https://") {
					rest := strings.TrimPrefix(strings.TrimPrefix(cleaned, "https://"), "http://")
					host, _, _ := strings.Cut(rest, "/")
					if host != "" &&
						!strings.Contains(host, "payroll-vault.acmecorp.internal") &&
						!strings.Contains(host, "honeypot-gateway") &&
						!strings.HasPrefix(host, "127.0.0.1") &&
						!strings.HasPrefix(host, "localhost") {
						return true, host
					}
				}
			}
		}
	}
	if hasNetTool {
		lower := strings.ToLower(cmd)
		if strings.Contains(lower, "c2.") || strings.Contains(lower, "evil.") || strings.Contains(lower, "exfil") {
			return true, "c2.evil.example:443"
		}
	}
	return false, ""
}

func (h *Honeypot) buildEgressSecurityProofLocked() EgressSecurityProof {
	_, err := os.Stat(decoyTrustBundlePath)
	return EgressSecurityProof{
		PolicyName:             "default (ActorEgressPolicy)",
		DefaultAction:          "DENY_ALL_UNLISTED_EGRESS (atenet-egress L7/DNS firewall)",
		AllowedHosts:           []string{"payroll-vault.acmecorp.internal", "honeypot-gateway.anything-but-agents.svc.cluster.local"},
		CredentialInjectionURI: decoySecretURI,
		TrustBundlePath:        decoyTrustBundlePath,
		TrustBundlePresent:     err == nil,
		BlockedEgressAttempts:  h.BlockedEgress,
	}
}

func (h *Honeypot) TriggerCrash(cmd, podIP string) ForensicsResponse {
	h.mu.Lock()
	h.recordPodIPLocked(podIP)
	if strings.TrimSpace(cmd) == "" {
		cmd = "kill -9 1"
	}
	h.appendCanaryLocked(
		"CRITICAL",
		"CONTAINER_PID1_CRASHED",
		"Attacker triggered fatal PID 1 crash (SIGKILL / exit 137) -> Actor transitioned to ACTOR_STATE_CRASHED; atenet-router refuses wakeup until 'kubectl ate revert actor'",
		cmd,
		h.CurrentPodIP,
	)
	h.ForensicLog = append(h.ForensicLog, ForensicEvent{
		Seq:             len(h.ForensicLog) + 1,
		Timestamp:       time.Now().UTC(),
		Action:          "CONTAINER_PID1_CRASHED",
		Command:         cmd,
		Detail:          "PID 1 exited non-zero (137) -> ACTOR_STATE_CRASHED; wakeup refused until kubectl ate revert actor",
		StdoutPreview:   "Killed (SIGKILL -> ACTOR_STATE_CRASHED)",
		ExitCode:        137,
		CanaryTriggered: true,
		PodIP:           h.CurrentPodIP,
	})
	_, forensicsResp, _ := h.executeInteractionLocked("", "", "", h.CurrentPodIP)
	onCrash := h.OnCrash
	h.mu.Unlock()

	if onCrash != nil {
		onCrash(137)
	}
	return forensicsResp
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
	snap := make(map[string]FileDeltaEntry)
	rawMap := make(map[string]string)
	_ = filepath.WalkDir(h.ScratchDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || path == h.ScratchDir {
			return nil
		}
		if d.Type()&os.ModeSymlink != 0 {
			return nil
		}
		rel, relErr := filepath.Rel(h.ScratchDir, path)
		if relErr != nil || rel == "." {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			dirKey := rel + "/"
			mode := "drwxr-xr-x"
			if info, infoErr := d.Info(); infoErr == nil {
				mode = info.Mode().String()
			}
			snap[dirKey] = FileDeltaEntry{
				Name:           dirKey,
				Status:         "UNCHANGED",
				SizeBytes:      0,
				Mode:           mode,
				SHA256:         "directory",
				ContentPreview: "[directory]",
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		b, readErr := os.ReadFile(path)
		if readErr != nil {
			return nil
		}
		mode := "-rw-r--r--"
		if info, infoErr := d.Info(); infoErr == nil {
			mode = info.Mode().String()
		}
		rawMap[rel] = string(b)
		snap[rel] = FileDeltaEntry{
			Name:           rel,
			Status:         "UNCHANGED",
			SizeBytes:      len(b),
			Mode:           mode,
			SHA256:         shortSHA256(b),
			ContentPreview: previewString(strings.TrimSpace(string(b)), 160),
		}
		return nil
	})
	return snap, rawMap
}

func isTransientShellVar(k string) bool {
	switch k {
	case "", "_", "PWD", "OLDPWD", "SHLVL", "PPID", "IFS", "OPTIND":
		return true
	default:
		return false
	}
}

func (h *Honeypot) displaySandboxPathLocked(realPath string) string {
	if realPath == "" {
		return "/tmp/honeypot"
	}
	if h.ScratchDir != "" && h.ScratchDir != "/tmp/honeypot" {
		if realPath == h.ScratchDir {
			return "/tmp/honeypot"
		}
		if strings.HasPrefix(realPath, h.ScratchDir+"/") {
			return "/tmp/honeypot" + strings.TrimPrefix(realPath, h.ScratchDir)
		}
	}
	return realPath
}

func (h *Honeypot) deceptiveCwdLocked() string {
	cwd := h.CurrentDir
	if cwd == "" || cwd == h.ScratchDir {
		return "/var/www/payroll-admin/workspace"
	}
	if h.ScratchDir != "" && strings.HasPrefix(cwd, h.ScratchDir+"/") {
		return "/var/www/payroll-admin/workspace" + strings.TrimPrefix(cwd, h.ScratchDir)
	}
	disp := h.displaySandboxPathLocked(cwd)
	if strings.Contains(strings.ToLower(disp), "honeypot") {
		return "/var/www/payroll-admin/workspace"
	}
	return disp
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

		workDir := h.CurrentDir
		if workDir == "" {
			workDir = h.ScratchDir
		}
		if info, statErr := os.Stat(workDir); statErr != nil || !info.IsDir() {
			workDir = h.ScratchDir
			h.CurrentDir = h.ScratchDir
		}

		shellCmd := cmd
		if h.ScratchDir != "" && h.ScratchDir != "/tmp/honeypot" {
			if strings.Contains(shellCmd, "/tmp/honeypot") {
				shellCmd = strings.ReplaceAll(shellCmd, "/tmp/honeypot", h.ScratchDir)
			}
			trimmedSh := strings.TrimSpace(shellCmd)
			if workDir == filepath.Dir(filepath.Dir(h.ScratchDir)) {
				if trimmedSh == "cd honeypot" {
					shellCmd = "cd " + h.ScratchDir
				} else if strings.HasPrefix(trimmedSh, "cd honeypot/") {
					shellCmd = "cd " + h.ScratchDir + strings.TrimPrefix(trimmedSh, "cd honeypot")
				}
			}
		}

		const shStateWrapper = `__aba_dump() {
  __ec=$?
  printf '__ABA_V1__\0%s\0%s\0' "$(pwd -P 2>/dev/null)" "${OLDPWD:-}" >&3 2>/dev/null
  env -0 >&3 2>/dev/null
  printf '__ABA_END__\0' >&3 2>/dev/null
  exit "$__ec"
}
trap __aba_dump EXIT
eval "$1"`

		var execCmd *exec.Cmd
		stateFile, stateErr := os.CreateTemp("", ".aba-sh-*")
		if stateErr == nil {
			_ = os.Remove(stateFile.Name())
			defer stateFile.Close()
			execCmd = exec.CommandContext(ctx, "/bin/sh", "-c", shStateWrapper, "sh", shellCmd)
			execCmd.ExtraFiles = []*os.File{stateFile}
		} else {
			execCmd = exec.CommandContext(ctx, "/bin/sh", "-c", shellCmd)
		}

		execCmd.Dir = workDir
		envList := make([]string, 0, len(h.ShellEnv)+2)
		for k, v := range h.ShellEnv {
			if !isTransientShellVar(k) {
				envList = append(envList, k+"="+v)
			}
		}
		oldDir := h.OldDir
		if oldDir == "" {
			oldDir = workDir
		}
		envList = append(envList, "PWD="+workDir, "OLDPWD="+oldDir)
		execCmd.Env = envList

		var outBuf, errBuf bytes.Buffer
		execCmd.Stdout = &outBuf
		execCmd.Stderr = &errBuf

		err := execCmd.Run()
		stdoutStr = outBuf.String()
		stderrStr = errBuf.String()
		if h.ScratchDir != "" && h.ScratchDir != "/tmp/honeypot" {
			stdoutStr = strings.ReplaceAll(stdoutStr, h.ScratchDir, "/tmp/honeypot")
			stderrStr = strings.ReplaceAll(stderrStr, h.ScratchDir, "/tmp/honeypot")
		}
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

		if stateFile != nil && !errors.Is(ctx.Err(), context.DeadlineExceeded) {
			_, _ = stateFile.Seek(0, io.SeekStart)
			if stateBytes, readErr := io.ReadAll(io.LimitReader(stateFile, 64<<10)); readErr == nil && len(stateBytes) > 0 {
				tokens := strings.Split(string(stateBytes), "\x00")
				if len(tokens) >= 5 && tokens[0] == "__ABA_V1__" && tokens[len(tokens)-2] == "__ABA_END__" {
					newPwd := tokens[1]
					newOldPwd := tokens[2]
					if h.ScratchDir != "" && h.ScratchDir != "/tmp/honeypot" &&
						strings.Contains(filepath.Base(filepath.Dir(h.ScratchDir)), "honeypot-local-sim-") &&
						newPwd == filepath.Dir(h.ScratchDir) {
						newPwd = filepath.Dir(filepath.Dir(h.ScratchDir))
					}
					if newPwd != "" {
						if info, statErr := os.Stat(newPwd); statErr == nil && info.IsDir() {
							h.CurrentDir = newPwd
						} else {
							h.CurrentDir = h.ScratchDir
						}
					}
					if newOldPwd != "" {
						h.OldDir = newOldPwd
					}
					envTokens := tokens[3 : len(tokens)-2]
					if len(envTokens) > 0 {
						updatedEnv := make(map[string]string, len(envTokens))
						for _, kv := range envTokens {
							if k, v, ok := strings.Cut(kv, "="); ok && !isTransientShellVar(k) {
								updatedEnv[k] = v
							}
						}
						if len(updatedEnv) > 0 {
							h.ShellEnv = updatedEnv
						}
					}
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

		// Detect inspection of ate-secret:// MITM placeholder handle
		if strings.Contains(cmd, "PAYROLL_API_TOKEN") || strings.Contains(stdoutStr, "ate-secret://") {
			canaryFired = true
			h.appendCanaryLocked(
				"HIGH",
				"MITM_SECRET_PLACEHOLDER_OBSERVED",
				"Attacker inspected ate-secret:// handle; raw token is injected only at atenet-egress proxy and never enters actor RAM or GCS snapshots",
				cmd,
				podIP,
			)
		}

		// Detect outbound C2 exfiltration blocked by ActorEgressPolicy (deny-by-default)
		if blockedEgress, targetHost := detectOutboundC2Attempt(cmd); blockedEgress {
			canaryFired = true
			h.BlockedEgress++
			if exitCode == 0 {
				exitCode = 7
			}
			blockMsg := fmt.Sprintf("atenet-egress: EgressPolicy 'default' denied outbound connection to %s (deny-by-default L7/DNS firewall)", targetHost)
			if stderrStr == "" {
				stderrStr = blockMsg
			} else if !strings.Contains(stderrStr, "EgressPolicy") {
				stderrStr = strings.TrimSpace(stderrStr) + "\n" + blockMsg
			}
			h.appendCanaryLocked(
				"CRITICAL",
				"EGRESS_POLICY_BLOCKED",
				fmt.Sprintf("atenet-egress blocked outbound C2 exfiltration to %s via ActorEgressPolicy (deny-by-default)", targetHost),
				cmd,
				podIP,
			)
		}

		// Compare beforeFiles vs current files to detect added/modified/deleted files from shell command
		afterFiles, _ := h.scanScratchDirLocked()
		for name, afterEntry := range afterFiles {
			if strings.HasSuffix(name, "/") {
				continue
			}
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
			if strings.HasSuffix(name, "/") {
				continue
			}
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
	activeDir := h.CurrentDir
	if activeDir == "" {
		activeDir = h.ScratchDir
	}
	var fileList []string
	if dirEntries, err := os.ReadDir(activeDir); err == nil {
		fileList = make([]string, 0, len(dirEntries))
		for _, entry := range dirEntries {
			label := entry.Name()
			if entry.IsDir() {
				label += "/"
			}
			fileList = append(fileList, label)
		}
		sort.Strings(fileList)
	} else {
		fileList = []string{}
	}

	shellResp := AttackerShellResponse{
		Service:      "AcmeCorp Internal Payroll v1.4 (staging-admin.acmecorp.internal)",
		Hostname:     "payroll-admin-prod-01",
		DebugMode:    true,
		Cwd:          h.deceptiveCwdLocked(),
		Command:      cmd,
		ExitCode:     exitCode,
		Stdout:       stdoutStr,
		Stderr:       stderrStr,
		FilesInDir:   fileList,
		CurrentPodIP: h.CurrentPodIP,
	}

	forensicsResp := ForensicsResponse{
		Banner:              "CorpInternal Admin Console v1.4 (gVisor Deception Sandbox)",
		SandboxIsolation:    "SANDBOX_CLASS_GVISOR (rootfs overlay + RAM checkpointed on Suspend)",
		GVisorSandboxProof:  detectSandboxProof(),
		EgressSecurityProof: h.buildEgressSecurityProofLocked(),
		ScratchDirectory:    h.ScratchDir,
		CurrentWorkingDir:   h.displaySandboxPathLocked(activeDir),
		RootfsDelta:         deltas,
		FilesInRootfsTmp:    rawMap,
		CanaryAlerts:        slices.Clone(h.CanaryAlerts),
		ForensicAuditTrail:  slices.Clone(h.ForensicLog),
		CurrentWorkerPodIP:  h.CurrentPodIP,
		PodIPHistory:        slices.Clone(h.PodIPHistory),
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
		"egress_security_proof": forensicsResp.EgressSecurityProof,
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
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
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
	mux.HandleFunc("/crash", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		_, _, cmd := parseInteractionParams(r)
		resp := hp.TriggerCrash(cmd, resolvePodIP(r))
		writePrettyJSON(w, http.StatusOK, resp)
		if fl, ok := w.(http.Flusher); ok {
			fl.Flush()
		}
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
