package aor

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/Siddhant-K-code/agentflow-infrastructure/internal/config"
	"github.com/Siddhant-K-code/agentflow-infrastructure/internal/db"
	"github.com/nats-io/nats.go"
	"github.com/redis/go-redis/v9"
)

type ControlPlane struct {
	cfg      *config.Config
	db       *db.PostgresDB
	redis    *redis.Client
	nats     *nats.Conn
	js       nats.JetStreamContext
	
	scheduler *Scheduler
	monitor   *Monitor
	
	mu       sync.RWMutex
	running  bool
	shutdown chan struct{}
}

func NewControlPlane(cfg *config.Config) (*ControlPlane, error) {
	// Initialize database
	pgDB, err := db.NewPostgresDB(&cfg.Database)
	if err != nil {
		return nil, fmt.Errorf("failed to initialize postgres: %w", err)
	}

	// Initialize Redis
	redisClient := redis.NewClient(&redis.Options{
		Addr:     fmt.Sprintf("%s:%d", cfg.Redis.Host, cfg.Redis.Port),
		Password: cfg.Redis.Password,
		DB:       cfg.Redis.DB,
	})

	// Initialize NATS
	nc, err := nats.Connect(cfg.NATS.URL)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to NATS: %w", err)
	}

	js, err := nc.JetStream()
	if err != nil {
		return nil, fmt.Errorf("failed to get JetStream context: %w", err)
	}

	cp := &ControlPlane{
		cfg:      cfg,
		db:       pgDB,
		redis:    redisClient,
		nats:     nc,
		js:       js,
		shutdown: make(chan struct{}),
	}

	// Initialize scheduler and monitor
	cp.scheduler = NewScheduler(cp)
	cp.monitor = NewMonitor(cp)

	return cp, nil
}

func (cp *ControlPlane) Start(ctx context.Context) error {
	cp.mu.Lock()
	defer cp.mu.Unlock()

	if cp.running {
		return fmt.Errorf("control plane already running")
	}

	// Run database migrations
	if err := cp.db.RunMigrations("./migrations"); err != nil {
		return fmt.Errorf("failed to run migrations: %w", err)
	}

	// Initialize NATS streams
	if err := cp.initStreams(); err != nil {
		return fmt.Errorf("failed to initialize streams: %w", err)
	}

	// Start scheduler
	if err := cp.scheduler.Start(ctx); err != nil {
		return fmt.Errorf("failed to start scheduler: %w", err)
	}

	// Start monitor
	if err := cp.monitor.Start(ctx); err != nil {
		return fmt.Errorf("failed to start monitor: %w", err)
	}

	cp.running = true
	log.Println("Control plane started")

	return nil
}

func (cp *ControlPlane) Shutdown(ctx context.Context) error {
	cp.mu.Lock()
	defer cp.mu.Unlock()

	if !cp.running {
		return nil
	}

	close(cp.shutdown)

	// Shutdown components
	if cp.scheduler != nil {
		cp.scheduler.Shutdown(ctx)
	}
	if cp.monitor != nil {
		cp.monitor.Shutdown(ctx)
	}

	// Close connections
	if cp.nats != nil {
		cp.nats.Close()
	}
	if cp.redis != nil {
		cp.redis.Close()
	}
	if cp.db != nil {
		cp.db.Close()
	}

	cp.running = false
	log.Println("Control plane shutdown complete")

	return nil
}

func (cp *ControlPlane) SubmitWorkflow(ctx context.Context, req *RunRequest) (*WorkflowRun, error) {
	// Get workflow spec
	spec, err := cp.getWorkflowSpec(ctx, req.WorkflowName, req.WorkflowVersion)
	if err != nil {
		return nil, fmt.Errorf("failed to get workflow spec: %w", err)
	}

	// Create workflow run
	run := &WorkflowRun{
		ID:             uuid.New(),
		WorkflowSpecID: spec.ID,
		Status:         RunStatusQueued,
		Metadata:       Metadata{
			"inputs":       req.Inputs,
			"tags":         req.Tags,
			"budget_cents": req.BudgetCents,
		},
		CreatedAt: time.Now(),
	}

	// Save to database
	if err := cp.saveWorkflowRun(ctx, run); err != nil {
		return nil, fmt.Errorf("failed to save workflow run: %w", err)
	}

	// Submit to scheduler
	if err := cp.scheduler.SubmitRun(ctx, run, spec); err != nil {
		return nil, fmt.Errorf("failed to submit run to scheduler: %w", err)
	}

	return run, nil
}

func (cp *ControlPlane) GetWorkflowRun(ctx context.Context, runID uuid.UUID) (*WorkflowRun, error) {
	query := `SELECT id, workflow_spec_id, status, started_at, ended_at, cost_cents, metadata, created_at 
			  FROM workflow_run WHERE id = $1`
	
	var run WorkflowRun
	var metadataJSON []byte
	
	err := cp.db.QueryRowContext(ctx, query, runID).Scan(
		&run.ID, &run.WorkflowSpecID, &run.Status, &run.StartedAt, &run.EndedAt,
		&run.CostCents, &metadataJSON, &run.CreatedAt,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to get workflow run: %w", err)
	}

	if err := json.Unmarshal(metadataJSON, &run.Metadata); err != nil {
		return nil, fmt.Errorf("failed to unmarshal metadata: %w", err)
	}

	return &run, nil
}

func (cp *ControlPlane) CancelWorkflowRun(ctx context.Context, runID uuid.UUID) error {
	// Update run status
	query := `UPDATE workflow_run SET status = 'canceled' WHERE id = $1 AND status IN ('queued', 'running')`
	
	result, err := cp.db.ExecContext(ctx, query, runID)
	if err != nil {
		return fmt.Errorf("failed to cancel workflow run: %w", err)
	}

	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("failed to get rows affected: %w", err)
	}

	if rowsAffected == 0 {
		return fmt.Errorf("workflow run not found or not in cancellable state")
	}

	// Send cancellation signal
	cancelMsg := map[string]interface{}{
		"run_id": runID.String(),
		"action": "cancel",
	}

	msgData, _ := json.Marshal(cancelMsg)
	if _, err := cp.js.Publish("agentflow.signals", msgData); err != nil {
		log.Printf("Failed to send cancellation signal: %v", err)
	}

	return nil
}

func (cp *ControlPlane) initStreams() error {
	streams := []struct {
		name     string
		subjects []string
	}{
		{"AGENTFLOW_TASKS", []string{"agentflow.tasks.*"}},
		{"AGENTFLOW_RESULTS", []string{"agentflow.results.*"}},
		{"AGENTFLOW_SIGNALS", []string{"agentflow.signals"}},
	}

	for _, stream := range streams {
		_, err := cp.js.AddStream(&nats.StreamConfig{
			Name:     stream.name,
			Subjects: stream.subjects,
			MaxAge:   24 * time.Hour,
		})
		if err != nil && err != nats.ErrStreamNameAlreadyInUse {
			return fmt.Errorf("failed to create stream %s: %w", stream.name, err)
		}
	}

	return nil
}

func (cp *ControlPlane) getWorkflowSpec(ctx context.Context, name string, version int) (*WorkflowSpec, error) {
	query := `SELECT id, org_id, name, version, dag, metadata 
			  FROM workflow_spec WHERE name = $1 AND version = $2`
	
	var spec WorkflowSpec
	var dagJSON, metadataJSON []byte
	
	err := cp.db.QueryRowContext(ctx, query, name, version).Scan(
		&spec.ID, &spec.OrgID, &spec.Name, &spec.Version, &dagJSON, &metadataJSON,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to get workflow spec: %w", err)
	}

	if err := json.Unmarshal(dagJSON, &spec.DAG); err != nil {
		return nil, fmt.Errorf("failed to unmarshal DAG: %w", err)
	}

	if err := json.Unmarshal(metadataJSON, &spec.Metadata); err != nil {
		return nil, fmt.Errorf("failed to unmarshal metadata: %w", err)
	}

	return &spec, nil
}

func (cp *ControlPlane) saveWorkflowRun(ctx context.Context, run *WorkflowRun) error {
	metadataJSON, err := json.Marshal(run.Metadata)
	if err != nil {
		return fmt.Errorf("failed to marshal metadata: %w", err)
	}

	query := `INSERT INTO workflow_run (id, workflow_spec_id, status, cost_cents, metadata, created_at)
			  VALUES ($1, $2, $3, $4, $5, $6)`
	
	_, err = cp.db.ExecContext(ctx, query,
		run.ID, run.WorkflowSpecID, run.Status, run.CostCents, metadataJSON, run.CreatedAt,
	)
	if err != nil {
		return fmt.Errorf("failed to insert workflow run: %w", err)
	}

	return nil
}