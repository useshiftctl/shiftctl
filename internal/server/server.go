package server

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/shiftctl/shiftctl/internal/assessment"
	"github.com/shiftctl/shiftctl/internal/credentials"
	"github.com/shiftctl/shiftctl/internal/destination"
	"github.com/shiftctl/shiftctl/internal/execstate"
	"github.com/shiftctl/shiftctl/internal/executor"
	"github.com/shiftctl/shiftctl/internal/migration"
	"github.com/shiftctl/shiftctl/internal/plan"
	"github.com/shiftctl/shiftctl/internal/verifier"
)

//go:embed web
var webFS embed.FS

// CombinedState is what the /api/state endpoint returns.
type CombinedState struct {
	MigrationName  string                  `json:"migration_name"`
	Screen         string                  `json:"screen"`
	Record         *migration.Record       `json:"record,omitempty"`
	Assessment     *assessment.State       `json:"assessment,omitempty"`
	Plan           *plan.Plan              `json:"plan,omitempty"`
	Execstate      *execstate.State        `json:"execstate,omitempty"`
	ActiveStage    int                     `json:"active_stage,omitempty"`
	ActiveProgress *executor.StageProgress `json:"active_progress,omitempty"`
	VerifyResults  *verifier.SavedReport   `json:"verify_results,omitempty"`
	Corrections    map[string]string       `json:"corrections,omitempty"`
	RepoFilter     []string                `json:"repo_filter,omitempty"`
	AssessRunning  bool                    `json:"assess_running,omitempty"`
	AssessError    string                  `json:"assess_error,omitempty"`
	ExecuteRunning bool                    `json:"execute_running,omitempty"`
	ExecuteError   string                  `json:"execute_error,omitempty"`
	VerifyRunning  bool                    `json:"verify_running,omitempty"`
	VerifyError    string                  `json:"verify_error,omitempty"`
}

// Server handles HTTP for the browser companion.
type Server struct {
	name           string
	mux            *http.ServeMux
	assessMu       sync.Mutex
	assessRunning  bool
	assessError    string
	executeMu      sync.Mutex
	executeRunning bool
	executeError   string
	verifyMu       sync.Mutex
	verifyRunning  bool
	verifyError    string
}

// New creates a Server for the given migration name.
func New(name string) *Server {
	s := &Server{name: name}

	sub, _ := fs.Sub(webFS, "web")
	static := http.FileServer(http.FS(sub))

	mux := http.NewServeMux()
	mux.HandleFunc("/api/state", s.handleState)
	mux.HandleFunc("/api/approve", s.handleApprove)
	mux.HandleFunc("/api/correction", s.handleCorrection)
	mux.HandleFunc("/api/plan/note", s.handlePlanNote)
	mux.HandleFunc("/api/disconnect", s.handleDisconnect)
	mux.HandleFunc("/api/decommission", s.handleDecommission)
	mux.HandleFunc("/api/export", s.handleExport)
	mux.HandleFunc("/api/assess", s.handleAssess)
	mux.HandleFunc("/api/plan", s.handlePlan)
	mux.HandleFunc("/api/execute", s.handleExecute)
	mux.HandleFunc("/api/destination-credential", s.handleDestinationCredential)
	mux.HandleFunc("/api/filter", s.handleFilter)
	mux.HandleFunc("/api/verify", s.handleVerify)
	mux.Handle("/", static)

	s.mux = mux
	return s
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	s.mux.ServeHTTP(w, r)
}

// ── /api/state ────────────────────────────────────────────────────────────────

func (s *Server) handleState(w http.ResponseWriter, r *http.Request) {
	state, err := s.buildState()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, state)
}

func (s *Server) buildState() (*CombinedState, error) {
	st := &CombinedState{MigrationName: s.name}

	// Populate in-memory running/error state.
	s.assessMu.Lock()
	st.AssessRunning = s.assessRunning
	st.AssessError = s.assessError
	s.assessMu.Unlock()

	s.executeMu.Lock()
	st.ExecuteRunning = s.executeRunning
	st.ExecuteError = s.executeError
	s.executeMu.Unlock()

	s.verifyMu.Lock()
	st.VerifyRunning = s.verifyRunning
	st.VerifyError = s.verifyError
	s.verifyMu.Unlock()

	rec, err := migration.Load(s.name)
	if err != nil {
		st.Screen = "first-run"
		return st, nil
	}
	st.Record = rec

	if rec.Status == migration.StatusComplete {
		// Load plan for the export/record display
		if p, err := plan.Load(s.name); err == nil {
			st.Plan = p
		}
		if exec, err := execstate.Load(s.name); err == nil {
			st.Execstate = exec
		}
		st.Screen = "decommission-complete"
		return st, nil
	}

	assess, _ := assessment.Load(s.name)
	st.Assessment = assess
	st.Corrections = s.loadCorrections()
	st.RepoFilter = loadFilter(s.name)

	if assess == nil {
		// If assessment is running, show the assessment screen so the UI can
		// display progress. Otherwise fall back to the setup screen.
		if st.AssessRunning {
			st.Screen = "assessment"
		} else {
			st.Screen = "setup"
		}
		return st, nil
	}

	p, _ := plan.Load(s.name)
	st.Plan = p

	if p == nil {
		st.Screen = "assessment"
		return st, nil
	}

	exec, _ := execstate.Load(s.name)
	st.Execstate = exec

	if exec == nil {
		st.Screen = "planning"
		return st, nil
	}

	// Determine active screen from exec state.
	screen, activeStage := deriveScreen(exec)
	st.Screen = screen
	st.ActiveStage = activeStage

	if activeStage > 0 {
		prog, _ := executor.LoadProgress(s.name, activeStage)
		st.ActiveProgress = prog

		if screen == "verifying" {
			vr, _ := verifier.LoadReport(s.name, activeStage)
			st.VerifyResults = vr
		}
	}

	return st, nil
}

// deriveScreen maps execution state to a screen name and active stage number.
func deriveScreen(exec *execstate.State) (screen string, activeStage int) {
	// First pass: executing / interrupted / verifying take priority.
	for _, ss := range exec.Stages {
		switch ss.Status {
		case execstate.StatusExecuting, execstate.StatusInterrupted:
			return "execution", ss.Number
		case execstate.StatusVerifying:
			return "verifying", ss.Number
		}
	}

	// Second pass: find next pending stage.
	for i, ss := range exec.Stages {
		if ss.Status == execstate.StatusPending {
			if i == 0 || exec.Stages[i-1].Status == execstate.StatusVerified {
				return "approval", ss.Number
			}
		}
	}

	// Check for approved (execute not yet started).
	for _, ss := range exec.Stages {
		if ss.Status == execstate.StatusApproved {
			return "execution", ss.Number
		}
	}

	// All verified?
	allVerified := true
	for _, ss := range exec.Stages {
		if ss.Status != execstate.StatusVerified {
			allVerified = false
			break
		}
	}
	if allVerified {
		return "decommission", 0
	}

	return "planning", 0
}

// ── /api/approve ──────────────────────────────────────────────────────────────

func (s *Server) handleApprove(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		StageNumber int `json:"stage_number"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	p, err := plan.Load(s.name)
	if err != nil {
		http.Error(w, "plan not found", http.StatusNotFound)
		return
	}
	exec, err := execstate.LoadOrInit(s.name, p)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Build ApprovedItem list from plan stage.
	var items []execstate.ApprovedItem
	for _, st := range p.Stages {
		if st.Number == body.StageNumber {
			for _, r := range st.Repos {
				items = append(items, execstate.ApprovedItem{
					FullName:   r.FullName,
					RiskLevel:  r.RiskLevel,
					Confidence: r.Confidence,
				})
			}
			break
		}
	}
	if len(items) == 0 {
		http.Error(w, fmt.Sprintf("stage %d not found in plan", body.StageNumber), http.StatusBadRequest)
		return
	}

	if _, err := execstate.RecordApproval(exec, body.StageNumber, items); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	writeJSON(w, map[string]any{"ok": true, "stage": body.StageNumber})
}

// ── /api/correction ───────────────────────────────────────────────────────────

func (s *Server) handleCorrection(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		FullName string `json:"full_name"`
		Note     string `json:"note"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	corrections := s.loadCorrections()
	if corrections == nil {
		corrections = make(map[string]string)
	}
	if body.Note == "" {
		delete(corrections, body.FullName)
	} else {
		corrections[body.FullName] = body.Note
	}
	if err := s.saveCorrections(corrections); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

// ── /api/plan/note ────────────────────────────────────────────────────────────

func (s *Server) handlePlanNote(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		StageNumber int    `json:"stage_number"`
		Note        string `json:"note"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	p, err := plan.Load(s.name)
	if err != nil {
		http.Error(w, "plan not found", http.StatusNotFound)
		return
	}
	for i := range p.Stages {
		if p.Stages[i].Number == body.StageNumber {
			p.Stages[i].Note = body.Note
			break
		}
	}
	if err := plan.Save(p); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

// ── /api/disconnect ───────────────────────────────────────────────────────────

func (s *Server) handleDisconnect(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	// Remove all local data files but keep record.json.
	if err := removeMigrationData(s.name); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

// ── /api/decommission ─────────────────────────────────────────────────────────

func (s *Server) handleDecommission(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Remove credentials (best-effort).
	_ = credentials.Delete(s.name, "source")
	_ = credentials.Delete(s.name, "destination")

	// Remove data files, keep record.json.
	_ = removeMigrationData(s.name)

	// Mark record complete.
	rec, err := migration.Load(s.name)
	if err != nil {
		http.Error(w, "migration not found", http.StatusNotFound)
		return
	}
	rec.Status = migration.StatusComplete
	if err := migration.Save(rec); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	writeJSON(w, map[string]any{"ok": true})
}

// ── /api/export ───────────────────────────────────────────────────────────────

func (s *Server) handleExport(w http.ResponseWriter, r *http.Request) {
	rec, err := migration.Load(s.name)
	if err != nil {
		http.Error(w, "migration not found", http.StatusNotFound)
		return
	}

	p, _ := plan.Load(s.name)
	exec, _ := execstate.Load(s.name)
	corrections := s.loadCorrections()

	md := buildMarkdownRecord(rec, p, exec, corrections)
	writeJSON(w, map[string]any{"markdown": md, "filename": exportFilename(rec)})
}

// ── Helpers ───────────────────────────────────────────────────────────────────

func (s *Server) correctionsPath() (string, error) {
	base, err := migration.ShiftctlDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "migrations", migration.SanitizeName(s.name), "corrections.json"), nil
}

func (s *Server) loadCorrections() map[string]string {
	path, err := s.correctionsPath()
	if err != nil {
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var m map[string]string
	_ = json.Unmarshal(data, &m)
	return m
}

func (s *Server) saveCorrections(m map[string]string) error {
	path, err := s.correctionsPath()
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0600)
}

func filterPath(name string) (string, error) {
	base, err := migration.ShiftctlDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "migrations", migration.SanitizeName(name), "filter.json"), nil
}

func loadFilter(name string) []string {
	path, err := filterPath(name)
	if err != nil {
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var repos []string
	_ = json.Unmarshal(data, &repos)
	return repos
}

func (s *Server) handleFilter(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		Repos []string `json:"repos"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	path, err := filterPath(s.name)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	data, err := json.MarshalIndent(body.Repos, "", "  ")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

func removeMigrationData(name string) error {
	base, err := migration.ShiftctlDir()
	if err != nil {
		return err
	}
	dir := filepath.Join(base, "migrations", migration.SanitizeName(name))
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	for _, e := range entries {
		if e.Name() == "record.json" {
			continue
		}
		_ = os.RemoveAll(filepath.Join(dir, e.Name()))
	}
	return nil
}

func buildMarkdownRecord(rec *migration.Record, p *plan.Plan, exec *execstate.State, corrections map[string]string) string {
	var sb strings.Builder
	sb.WriteString("# Migration Record\n\n")
	sb.WriteString(fmt.Sprintf("Migration: %s → %s\n", rec.SourcePlatform, rec.DestinationName))
	sb.WriteString(fmt.Sprintf("Started: %s\n", rec.CreatedAt.Format(time.RFC3339)))
	sb.WriteString(fmt.Sprintf("Status: %s\n\n", rec.Status))

	if p == nil {
		return sb.String()
	}

	sb.WriteString("## Summary\n\n")
	total := 0
	for _, s := range p.Stages {
		total += len(s.Repos)
	}
	sb.WriteString(fmt.Sprintf("%d items across %d stages.\n\n", total, len(p.Stages)))

	for _, stage := range p.Stages {
		sb.WriteString(fmt.Sprintf("## Stage %d — %s (%d items)\n\n", stage.Number, stage.Label, len(stage.Repos)))
		if exec != nil {
			for _, ss := range exec.Approvals {
				if ss.StageNumber == stage.Number {
					sb.WriteString(fmt.Sprintf("Approved: %s\n\n", ss.ApprovedAt.Format(time.RFC3339)))
					break
				}
			}
		}
		for _, repo := range stage.Repos {
			source := "Agent analysis"
			if corrections[repo.FullName] != "" {
				source = "User correction"
			}
			sb.WriteString(fmt.Sprintf("- %s — %s, %s risk — %s\n", repo.FullName, repo.Confidence, repo.RiskLevel, source))
		}
		sb.WriteString("\n")
	}

	sb.WriteString("---\n*No raw file contents from your repos are included, only structured findings.*\n")
	return sb.String()
}

func exportFilename(rec *migration.Record) string {
	src := strings.ReplaceAll(rec.SourcePlatform, " ", "-")
	dst := strings.ReplaceAll(strings.ToLower(rec.DestinationName), " ", "-")
	return fmt.Sprintf("shiftctl-migration-record-%s-%s.md", src, dst)
}

// ── /api/assess ───────────────────────────────────────────────────────────────

func (s *Server) handleAssess(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	apiKey := os.Getenv("ANTHROPIC_API_KEY")
	if apiKey == "" {
		writeJSONStatus(w, http.StatusBadRequest, map[string]any{
			"error": "ANTHROPIC_API_KEY not set",
			"hint":  "export ANTHROPIC_API_KEY=sk-ant-... then restart shiftctl serve",
		})
		return
	}
	workspaceID := os.Getenv("ANTHROPIC_WORKSPACE_ID")

	token, err := credentials.Get(s.name, "source")
	if err != nil {
		writeJSONStatus(w, http.StatusBadRequest, map[string]any{"error": "source credential not found: " + err.Error()})
		return
	}

	rec, err := migration.Load(s.name)
	if err != nil {
		writeJSONStatus(w, http.StatusBadRequest, map[string]any{"error": "migration not found: " + err.Error()})
		return
	}

	s.assessMu.Lock()
	if s.assessRunning {
		s.assessMu.Unlock()
		writeJSONStatus(w, http.StatusConflict, map[string]any{"error": "assessment already running"})
		return
	}
	s.assessRunning = true
	s.assessError = ""
	s.assessMu.Unlock()

	repoFilter := loadFilter(s.name)

	go func() {
		defer func() {
			s.assessMu.Lock()
			s.assessRunning = false
			s.assessMu.Unlock()
		}()
		_, err := assessment.Run(context.Background(), rec, token, apiKey, workspaceID, repoFilter, nil)
		if err != nil {
			s.assessMu.Lock()
			s.assessError = err.Error()
			s.assessMu.Unlock()
		}
	}()

	writeJSON(w, map[string]any{"ok": true})
}

// ── /api/plan ─────────────────────────────────────────────────────────────────

func (s *Server) handlePlan(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	assess, err := assessment.Load(s.name)
	if err != nil || assess == nil || assess.Analysis == nil {
		writeJSONStatus(w, http.StatusBadRequest, map[string]any{"error": "assessment not complete"})
		return
	}

	p, err := plan.Build(assess)
	if err != nil {
		writeJSONStatus(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	if err := plan.Save(p); err != nil {
		writeJSONStatus(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}

	writeJSON(w, map[string]any{"ok": true})
}

// ── /api/execute ──────────────────────────────────────────────────────────────

func (s *Server) handleExecute(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	s.executeMu.Lock()
	if s.executeRunning {
		s.executeMu.Unlock()
		writeJSONStatus(w, http.StatusConflict, map[string]any{"error": "execution already running"})
		return
	}
	s.executeMu.Unlock()

	p, err := plan.Load(s.name)
	if err != nil {
		writeJSONStatus(w, http.StatusBadRequest, map[string]any{"error": "plan not found"})
		return
	}

	exec, err := execstate.LoadOrInit(s.name, p)
	if err != nil {
		writeJSONStatus(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}

	// Find the approved/executing/interrupted stage to run.
	var activeStage *plan.Stage
	var activeStageState *execstate.StageState
	for i := range exec.Stages {
		ss := &exec.Stages[i]
		if ss.Status == execstate.StatusApproved || ss.Status == execstate.StatusExecuting || ss.Status == execstate.StatusInterrupted {
			activeStageState = ss
			for j := range p.Stages {
				if p.Stages[j].Number == ss.Number {
					stageCopy := p.Stages[j]
					activeStage = &stageCopy
					break
				}
			}
			break
		}
	}

	if activeStage == nil || activeStageState == nil {
		writeJSONStatus(w, http.StatusBadRequest, map[string]any{"error": "no approved stage found to execute"})
		return
	}

	if err := executor.CheckGitAvailable(); err != nil {
		writeJSONStatus(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}

	srcCred, err := credentials.Get(s.name, "source")
	if err != nil {
		writeJSONStatus(w, http.StatusBadRequest, map[string]any{"error": "source credential not found: " + err.Error()})
		return
	}

	rec, err := migration.Load(s.name)
	if err != nil {
		writeJSONStatus(w, http.StatusBadRequest, map[string]any{"error": "migration not found: " + err.Error()})
		return
	}

	dest := destination.ForName(rec.DestinationName)
	if dest == nil {
		writeJSONStatus(w, http.StatusBadRequest, map[string]any{"error": "unknown destination: " + rec.DestinationName})
		return
	}

	dstCred, err := credentials.Get(s.name, "destination")
	if err != nil {
		writeJSONStatus(w, http.StatusBadRequest, map[string]any{
			"error":        "destination_credential_required",
			"instructions": dest.CredentialInstructions(),
			"prompt":       dest.CredentialPrompt(),
		})
		return
	}

	ctx := context.Background()
	username, err := dest.ValidateCredential(ctx, dstCred)
	if err != nil {
		writeJSONStatus(w, http.StatusBadRequest, map[string]any{
			"error":        "destination_credential_required",
			"instructions": dest.CredentialInstructions(),
			"prompt":       dest.CredentialPrompt(),
		})
		return
	}

	stageNum := activeStage.Number
	stageCopy := *activeStage

	s.executeMu.Lock()
	s.executeRunning = true
	s.executeError = ""
	s.executeMu.Unlock()

	go func() {
		defer func() {
			s.executeMu.Lock()
			s.executeRunning = false
			s.executeMu.Unlock()
		}()

		// Reload exec state inside goroutine to get latest.
		execState, err := execstate.Load(s.name)
		if err != nil {
			s.executeMu.Lock()
			s.executeError = err.Error()
			s.executeMu.Unlock()
			return
		}

		// Transition to executing.
		_ = execstate.Transition(execState, stageNum, execstate.StatusExecuting)

		opts := executor.Options{
			SourcePlatform:   rec.SourcePlatform,
			SourceCredential: srcCred,
			DestCredential:   dstCred,
			Dest:             dest,
			DestNamespace:    username,
		}

		_, runErr := executor.Run(ctx, execState, stageCopy, opts)

		// Reload for transition.
		execState2, loadErr := execstate.Load(s.name)
		if loadErr == nil {
			if runErr != nil {
				_ = execstate.Transition(execState2, stageNum, execstate.StatusInterrupted)
				s.executeMu.Lock()
				s.executeError = runErr.Error()
				s.executeMu.Unlock()
			} else {
				_ = execstate.Transition(execState2, stageNum, execstate.StatusVerifying)
			}
		} else if runErr != nil {
			s.executeMu.Lock()
			s.executeError = runErr.Error()
			s.executeMu.Unlock()
		}
	}()

	writeJSON(w, map[string]any{"ok": true})
}

// ── /api/destination-credential ───────────────────────────────────────────────

func (s *Server) handleDestinationCredential(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var body struct {
		Credential string `json:"credential"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Credential == "" {
		writeJSONStatus(w, http.StatusBadRequest, map[string]any{"error": "credential is required"})
		return
	}

	rec, err := migration.Load(s.name)
	if err != nil {
		writeJSONStatus(w, http.StatusBadRequest, map[string]any{"error": "migration not found"})
		return
	}

	dest := destination.ForName(rec.DestinationName)
	if dest == nil {
		writeJSONStatus(w, http.StatusBadRequest, map[string]any{"error": "unknown destination: " + rec.DestinationName})
		return
	}

	ctx := context.Background()
	username, err := dest.ValidateCredential(ctx, body.Credential)
	if err != nil {
		writeJSONStatus(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}

	if err := credentials.Set(s.name, "destination", body.Credential); err != nil {
		writeJSONStatus(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}

	writeJSON(w, map[string]any{"ok": true, "username": username})
}

// ── /api/verify ───────────────────────────────────────────────────────────────

func (s *Server) handleVerify(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	s.verifyMu.Lock()
	if s.verifyRunning {
		s.verifyMu.Unlock()
		writeJSONStatus(w, http.StatusConflict, map[string]any{"error": "verification already running"})
		return
	}
	s.verifyMu.Unlock()

	p, err := plan.Load(s.name)
	if err != nil {
		writeJSONStatus(w, http.StatusBadRequest, map[string]any{"error": "plan not found"})
		return
	}

	exec, err := execstate.Load(s.name)
	if err != nil {
		writeJSONStatus(w, http.StatusBadRequest, map[string]any{"error": "execution state not found"})
		return
	}

	// Find the stage in verifying state.
	var activeStage *plan.Stage
	var activeStageNum int
	for _, ss := range exec.Stages {
		if ss.Status == execstate.StatusVerifying {
			for j := range p.Stages {
				if p.Stages[j].Number == ss.Number {
					stageCopy := p.Stages[j]
					activeStage = &stageCopy
					activeStageNum = ss.Number
					break
				}
			}
			break
		}
	}
	if activeStage == nil {
		writeJSONStatus(w, http.StatusBadRequest, map[string]any{"error": "no stage in verifying state"})
		return
	}

	if err := executor.CheckGitAvailable(); err != nil {
		writeJSONStatus(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}

	rec, err := migration.Load(s.name)
	if err != nil {
		writeJSONStatus(w, http.StatusBadRequest, map[string]any{"error": "migration not found: " + err.Error()})
		return
	}

	dest := destination.ForName(rec.DestinationName)
	if dest == nil {
		writeJSONStatus(w, http.StatusBadRequest, map[string]any{"error": "unknown destination: " + rec.DestinationName})
		return
	}

	dstCred, err := credentials.Get(s.name, "destination")
	if err != nil {
		writeJSONStatus(w, http.StatusBadRequest, map[string]any{"error": "destination credential not found: " + err.Error()})
		return
	}

	ctx := context.Background()
	username, err := dest.ValidateCredential(ctx, dstCred)
	if err != nil {
		writeJSONStatus(w, http.StatusBadRequest, map[string]any{"error": "destination credential invalid: " + err.Error()})
		return
	}

	prog, err := executor.LoadProgress(s.name, activeStageNum)
	if err != nil {
		writeJSONStatus(w, http.StatusBadRequest, map[string]any{"error": fmt.Sprintf("no execution progress for stage %d: %v", activeStageNum, err)})
		return
	}

	stageCopy := *activeStage
	stageNum := activeStageNum

	s.verifyMu.Lock()
	s.verifyRunning = true
	s.verifyError = ""
	s.verifyMu.Unlock()

	go func() {
		defer func() {
			s.verifyMu.Lock()
			s.verifyRunning = false
			s.verifyMu.Unlock()
		}()

		opts := verifier.Options{
			DestCredential: dstCred,
			Dest:           dest,
			DestNamespace:  username,
		}

		report, err := verifier.Run(ctx, stageCopy, prog, opts)
		if err != nil {
			s.verifyMu.Lock()
			s.verifyError = err.Error()
			s.verifyMu.Unlock()
			return
		}

		// Always save the report so the UI can display results.
		_ = verifier.SaveReport(s.name, report)

		// Transition stage to verified regardless of individual item failures —
		// failures are surfaced as informational anomalies in the UI.
		execState, loadErr := execstate.Load(s.name)
		if loadErr == nil {
			_ = execstate.Transition(execState, stageNum, execstate.StatusVerified)
		}
	}()

	writeJSON(w, map[string]any{"ok": true})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func writeJSONStatus(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
