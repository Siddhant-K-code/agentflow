package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"

	"github.com/Siddhant-K-code/agentflow-infrastructure/internal/aor"
	"github.com/Siddhant-K-code/agentflow-infrastructure/internal/cas"
	"github.com/Siddhant-K-code/agentflow-infrastructure/internal/common"
	"github.com/Siddhant-K-code/agentflow-infrastructure/internal/pop"
	"github.com/Siddhant-K-code/agentflow-infrastructure/internal/scl"
	"github.com/gin-gonic/gin"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

var (
	port    = flag.String("port", "8080", "HTTP port to listen on")
	dbURL   = flag.String("db", "", "Database connection URL")
	debug   = flag.Bool("debug", false, "Enable debug mode")
)

func main() {
	flag.Parse()

	if *dbURL == "" {
		*dbURL = os.Getenv("DATABASE_URL")
		if *dbURL == "" {
			log.Fatal("DATABASE_URL must be set")
		}
	}

	// Initialize database
	db, err := gorm.Open(postgres.Open(*dbURL), &gorm.Config{})
	if err != nil {
		log.Fatalf("Failed to connect to database: %v", err)
	}

	// Auto-migrate database schema
	if err := migrateDatabase(db); err != nil {
		log.Fatalf("Failed to migrate database: %v", err)
	}

	// Initialize services
	aorService := aor.NewService(db)
	popService := pop.NewService(db)
	sclService := scl.NewService(db)

	// Setup HTTP router
	if !*debug {
		gin.SetMode(gin.ReleaseMode)
	}
	
	router := gin.Default()
	
	// Serve static files for web dashboard
	router.Static("/static", "./web/static")
	router.GET("/", func(c *gin.Context) {
		c.Redirect(http.StatusMovedPermanently, "/static/index.html")
	})
	
	// Health check
	router.GET("/health", func(c *gin.Context) {
		// Check database connection
		sqlDB, err := db.DB()
		dbStatus := "ok"
		if err != nil || sqlDB.Ping() != nil {
			dbStatus = "error"
		}

		status := gin.H{
			"status":   "ok",
			"database": dbStatus,
			"queue":    "ok", // TODO: Check NATS connection
			"workers":  0,    // TODO: Count active workers
		}

		if dbStatus == "error" {
			status["status"] = "degraded"
		}

		c.JSON(http.StatusOK, status)
	})

	// Setup API routes
	setupAORRoutes(router, aorService)
	setupPOPRoutes(router, popService)
	setupSCLRoutes(router, sclService)

	// Start server
	addr := fmt.Sprintf(":%s", *port)
	log.Printf("Starting AgentFlow server on %s", addr)
	
	if err := router.Run(addr); err != nil {
		log.Fatalf("Failed to start server: %v", err)
	}
}

func migrateDatabase(db *gorm.DB) error {
	return db.AutoMigrate(
		&common.Organization{},
		&common.Project{},
		&aor.WorkflowSpec{},
		&aor.WorkflowRun{},
		&aor.StepRun{},
		&pop.PromptTemplate{},
		&pop.PromptSuite{},
		&pop.PromptDeployment{},
		&scl.ContextBundle{},
		&cas.BudgetConfig{},
		&cas.ProviderConfig{},
	)
}

func setupAORRoutes(router *gin.Engine, service *aor.Service) {
	v1 := router.Group("/api/v1")
	
	// Workflow management
	v1.POST("/workflows/:name/versions", service.CreateWorkflowSpec)
	v1.GET("/workflows/:name/versions/:version", service.GetWorkflowSpec)
	v1.GET("/workflows/:name", service.GetLatestWorkflowSpec)
	v1.GET("/workflows", service.ListWorkflows)
	
	// Run management
	v1.POST("/runs", service.CreateRun)
	v1.GET("/runs/:id", service.GetRun)
	v1.GET("/runs", service.ListRuns)
	v1.POST("/runs/:id/cancel", service.CancelRun)
	v1.POST("/signals/:run_id", service.SendSignal)
	
	// Worker APIs
	v1.POST("/tasks/heartbeat", service.WorkerHeartbeat)
	v1.POST("/tasks/complete", service.CompleteTask)
}

func setupPOPRoutes(router *gin.Engine, service *pop.Service) {
	v1 := router.Group("/api/v1")
	
	// Prompt management
	v1.POST("/prompts/:name/versions", service.CreatePromptVersion)
	v1.GET("/prompts/:name", service.GetPrompt)
	v1.GET("/prompts/:name/versions/:version", service.GetPromptVersion)
	
	// Evaluation
	v1.POST("/prompts/:name/evaluate", service.EvaluatePrompt)
	
	// Deployment
	v1.POST("/deployments", service.DeployPrompt)
	v1.GET("/deployments/:name", service.GetDeployment)
}

func setupSCLRoutes(router *gin.Engine, service *scl.Service) {
	v1 := router.Group("/api/v1")
	
	// Context management
	v1.POST("/context/ingest", service.IngestContext)
	v1.POST("/context/prepare", service.PrepareContext)
	v1.POST("/context/policies/test", service.TestPolicy)
}