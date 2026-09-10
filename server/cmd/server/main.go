// Package main 是服务器进程入口。
//
// 引入动机：Phase 1 需要一个可启动的服务器进程，完成配置加载、日志初始化、
// 数据库连接建立、迁移执行、HTTP 路由注册以及受控优雅关闭。
//
// 运行模式：
//   - 默认（无 flag）：加载配置 → 连接数据库 → 执行迁移 → 启动 HTTP → 等待信号 → 优雅关闭
//   - -migrate-status：加载配置 → 连接数据库 → 打印当前迁移版本 → 退出
//     不执行迁移、不启动 HTTP、不等待信号。
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"partitura/server/internal/admin"
	"partitura/server/internal/audit"
	"partitura/server/internal/auth"
	"partitura/server/internal/bootstrap"
	"partitura/server/internal/config"
	"partitura/server/internal/crypto"
	"partitura/server/internal/db"
	"partitura/server/internal/db/migration"
	"partitura/server/internal/document"
	"partitura/server/internal/es"
	"partitura/server/internal/evaluation"
	"partitura/server/internal/health"
	httpmw "partitura/server/internal/http"
	"partitura/server/internal/job"
	"partitura/server/internal/profile"
	"partitura/server/internal/provider"
	"partitura/server/internal/scheduler"
	"partitura/server/internal/search"
	"partitura/server/internal/search/pipeline"
	"partitura/server/internal/settings"
	"partitura/server/internal/workspace"

	"github.com/google/uuid"
)

func main() {
	if err := run(); err != nil {
		slog.Error("服务器启动失败", "error", err)
		os.Exit(1)
	}
}

// run 执行服务器的主生命周期。
// 根据 -migrate-status flag 决定运行模式：
//   - migrate-status=true：仅查询并打印当前迁移版本后退出
//   - migrate-status=false（默认）：执行迁移 → 等待信号 → 优雅关闭
func run() error {
	migrateStatus := flag.Bool("migrate-status", false, "仅查询当前迁移版本并退出，不执行迁移或启动服务")
	flag.Parse()

	// 1. 加载配置
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("加载配置: %w", err)
	}

	// 2. 初始化结构化日志
	initLogger(cfg.LogLevel)
	slog.Info("配置加载完成", "http_addr", cfg.HTTPAddr, "log_level", cfg.LogLevel)

	// 3. 创建带超时的启动 context（仅用于启动阶段：DB 连接、迁移等）
	startCtx, startCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer startCancel()

	// 4. 建立 PostgreSQL 连接池
	database, err := db.New(startCtx, cfg.DSN(), 25, 5, 5*time.Minute)
	if err != nil {
		return fmt.Errorf("建立数据库连接: %w", err)
	}
	defer db.Close(database)

	// 5. 加载迁移文件
	migrationsDir := findMigrationsDir()
	slog.Info("加载迁移文件", "dir", migrationsDir)

	migrations, err := migration.LoadMigrations(migrationsDir)
	if err != nil {
		return fmt.Errorf("加载迁移文件: %w", err)
	}

	runner := migration.NewRunner(database, migrations)

	// 6. 根据 mode 分流
	if *migrateStatus {
		return runMigrateStatus(startCtx, runner)
	}

	// 运行阶段使用独立 context，不受启动超时影响。
	// Phase 6 修复：原实现将 startCtx（30s 超时）传入 runServer，
	// 导致 signal.NotifyContext 基于 startCtx 派生，30s 后 startCtx 超时
	// 触发优雅关闭，服务器无限循环重启。
	runCtx, runCancel := context.WithCancel(context.Background())
	defer runCancel()
	return runServer(runCtx, runner, cfg, database)
}

// runMigrateStatus 仅查询并打印当前迁移版本，不执行迁移、不启动 HTTP、不等待信号。
// 引入动机：运维需要独立检查数据库迁移状态而不触发迁移或启动服务。
func runMigrateStatus(ctx context.Context, runner *migration.Runner) error {
	version, err := runner.CurrentVersion(ctx)
	if err != nil {
		return fmt.Errorf("查询迁移版本: %w", err)
	}
	fmt.Printf("当前迁移版本: %d\n", version)
	return nil
}

// runServer 执行完整的服务器生命周期：执行迁移 → 启动 HTTP → 等待信号 → 优雅关闭。
func runServer(ctx context.Context, runner *migration.Runner, cfg *config.Config, database *sql.DB) error {
	executed, err := runner.Up(ctx)
	if err != nil {
		return fmt.Errorf("执行迁移: %w", err)
	}

	version, err := runner.CurrentVersion(ctx)
	if err != nil {
		return fmt.Errorf("查询迁移版本: %w", err)
	}
	slog.Info("迁移完成", "executed", executed, "current_version", version)

	// 构建 auth 配置
	authCfg := auth.AuthConfig{
		SessionDuration:            cfg.SessionDuration,
		DeviceAccessTokenDuration:  cfg.DeviceAccessTokenDuration,
		DeviceRefreshTokenDuration: cfg.DeviceRefreshTokenDuration,
		CookieSecure:               cfg.CookieSecure,
		CookieDomain:               cfg.CookieDomain,
		CookiePath:                 cfg.CookiePath,
		CookieName:                 cfg.CookieName,
		CSRFCookieName:             cfg.CSRFCookieName,
		CSRFHeaderName:             cfg.CSRFHeaderName,
		Argon2Memory:               cfg.Argon2Memory,
		Argon2Iterations:           cfg.Argon2Iterations,
		Argon2Parallelism:          cfg.Argon2Parallelism,
		Argon2SaltLength:           cfg.Argon2SaltLength,
		Argon2KeyLength:            cfg.Argon2KeyLength,
	}

	// 创建 auth repository 和 handler
	repo := auth.NewPGRepository(database)
	authHandler := auth.NewHandler(repo, authCfg)

	// 创建 device auth repository、audit repository 和 handler
	// PUBLIC_ORIGIN 通过配置注入，device authorization 的 verification_url 禁止从请求 Host 推导。
	daRepo := auth.NewPGDeviceAuthRepository(database)
	daCfg := auth.DefaultDeviceAuthConfig()
	daCfg.PublicOrigin = cfg.PublicOrigin
	auditRepo := audit.NewPGRepository(database)
	daHandler := auth.NewDeviceAuthHandler(daRepo, repo, authCfg, daCfg, auditRepo)

	// 创建 workspace repository 和 handler
	wsRepo := workspace.NewPGRepository(database)

	// 创建 document repository
	docRepo := document.NewPGRepository(database)

	// 创建 job repository（document handler 需要注入以 enqueue index job）
	jobRepo := job.NewPGRepository(database)

	// 创建 workspace handler，注入 document 初始化器以保证 workspace 创建时原子创建特殊文件
	// C1：jobRepo 同时实现 document.TxJobEnqueuer 和 workspace.TxJobEnqueuer（结构化接口），
	// 但 document.PGRepository.CreateSpecialDocuments 接收 document.TxJobEnqueuer，
	// 而 workspace.SpecialDocumentsInitializer 期望 workspace.TxJobEnqueuer，
	// 两个命名接口类型不兼容，需要适配器桥接。
	wsHandler := workspace.NewHandlerWithInitializer(wsRepo, auditRepo, &specialDocsInitAdapter{docRepo: docRepo}, jobRepo)
	adminHandler := workspace.NewAdminHandlerWithAuth(wsRepo, auditRepo, repo, authCfg)

	// 创建 document handler，注入 jobRepo 以在文档写入后 enqueue index_document job（C1）
	docHandler := document.NewHandler(docRepo, auditRepo, jobRepo)

	// --- Phase 3 搜索组件初始化 ---
	// 引入动机：design/01-SEARCH.md 要求完整搜索管线、ES、Provider、Profile、Job、Evaluation。

	// ES 客户端（可选，缺失时搜索降级）
	var esClient es.Client
	if cfg.ESURL != "" {
		esClient = es.NewHTTPClient(cfg.ESURL, 30*time.Second)
		slog.Info("ES 客户端已初始化", "url", cfg.ESURL)
	} else {
		slog.Warn("ES_URL 未配置，搜索将返回 degraded 响应")
	}

	// --- Provider Registry 初始化 ---
	// 引入动机：计划要求 Provider 配置从数据库加载，线程安全原子热更新。
	// 根密钥由部署方通过 MASTER_ENCRYPTION_KEY 环境变量提供。
	var rootKey []byte
	if cfg.MasterEncryptionKey != "" {
		rk, err := crypto.ParseRootKey(cfg.MasterEncryptionKey)
		if err != nil {
			return fmt.Errorf("解析 MASTER_ENCRYPTION_KEY: %w", err)
		}
		rootKey = rk
		slog.Info("根加密密钥已加载")
	} else {
		slog.Warn("MASTER_ENCRYPTION_KEY 未配置——若数据库已有 Provider 密文将拒绝启动")
	}

	providerRepo := provider.NewPGRepository(database)
	providerRegistry := provider.NewRegistry(providerRepo, rootKey)

	// 从数据库加载 Provider 配置
	// 引入动机：启动时从持久化配置恢复 Provider 实例。
	// 若数据库有密文但根密钥缺失/非法/解密失败，fail-fast 拒绝启动。
	if err := providerRegistry.LoadFromDB(ctx); err != nil {
		return fmt.Errorf("加载 Provider 配置: %w", err)
	}

	// 获取当前 Provider 实例（可能为 nil，表示未配置）
	embProvider := providerRegistry.GetEmbeddingProvider()
	rrProvider := providerRegistry.GetRerankerProvider()

	if embProvider != nil {
		slog.Info("Embedding Provider 已初始化（从数据库）")
	} else {
		slog.Warn("Embedding Provider 未配置，搜索将降级为 lexical-only")
	}
	if rrProvider != nil {
		slog.Info("Reranker Provider 已初始化（从数据库）")
	} else {
		slog.Warn("Reranker Provider 未配置，搜索将直接返回 RRF 结果")
	}

	// 搜索管线
	// 引入动机：使用 Registry 支持的管线，使搜索每次操作从 Registry 获取当前 Provider。
	pipe := pipeline.NewPipelineWithRegistry(
		esClient,
		providerRegistry.GetEmbeddingProvider,
		providerRegistry.GetRerankerProvider,
	)

	// Profile Repository
	profileRepo := profile.NewPGRepository(database)

	// 确保 active profile 存在
	if err := profile.EnsureDefaultProfile(ctx, profileRepo, "00000000-0000-0000-0000-000000000000"); err != nil {
		slog.Warn("确保默认 profile 失败（可能 PG 未就绪）", "error", err)
	}

	// 搜索指标 Repository
	metricsRepo := job.NewPGSearchMetricsRepo(database)

	// Evaluation Repository（C2：Evaluation API 完整实现）
	evalRepo := evaluation.NewPGRepository(database)
	evalResultRepo := evaluation.NewPGResultRepository(database)

	// 搜索 handler
	searchHandler := search.NewHandler(pipe, profileRepo, metricsRepo)

	// Admin handler
	adminSearchHandler := admin.NewHandler(profileRepo, jobRepo, auditRepo, evalRepo, evalResultRepo)

	// 注册路由
	mux := http.NewServeMux()
	auth.RegisterRoutes(mux, authHandler, repo, authCfg)
	auth.RegisterDeviceAuthRoutes(mux, daHandler, repo, authCfg)
	workspace.RegisterRoutes(mux, wsHandler, adminHandler, repo, authCfg)
	document.RegisterRoutes(mux, docHandler, wsRepo, repo, authCfg)
	search.RegisterRoutes(mux, searchHandler, wsRepo, repo, authCfg)
	admin.RegisterRoutes(mux, adminSearchHandler, repo, authCfg)

	// Bootstrap 路由（公开，不需要认证——仅 users 表为空时可用）
	// 引入动机：计划要求空数据库启动时提供一次性 Bootstrap API 创建第一个 system_admin。
	bootstrapHandler := bootstrap.NewHandler(repo, repo, auditRepo, authCfg)
	bootstrap.RegisterRoutes(mux, bootstrapHandler)

	// Provider 管理 API 路由（system_admin + CSRF）
	// 引入动机：计划要求管理员通过 Web API 管理 Provider 配置。
	providerHandler := provider.NewHandler(providerRegistry, providerRepo, auditRepo)
	provider.RegisterRoutes(mux, providerHandler, repo, authCfg)

	// 注册健康检查路由
	// 引入动机：design/05-OPERATIONS.md §Health 要求 /healthz 和 /readyz 端点。
	// 这些路由不需要认证，直接注册在 mux 上。
	mux.HandleFunc("/healthz", health.HealthzHandler)

	// 构造 readyz 健康快照采集器
	// 引入动机：readyz 需要注入 PG、ES、Embedding、Reranker、job stats 和 profile 查询依赖。
	// Provider 从 Registry 函数引用动态获取，支持热更新——管理员保存新 Provider 后健康检查立即反映。
	healthCollector := health.NewSnapshotCollector(
		&health.PGPingAdapter{DB: database},
		&health.ESPingAdapter{Client: esClient},
		&health.EmbeddingCheckAdapter{ProviderFn: providerRegistry.GetEmbeddingProvider},
		&health.RerankerCheckAdapter{ProviderFn: providerRegistry.GetRerankerProvider},
		jobRepo,
		&health.ProfileQueryAdapter{Repo: profileRepo},
	)
	mux.HandleFunc("/readyz", health.ReadyHandler(healthCollector))

	// Settings 管理 handler
	// 引入动机：Phase6 WP3 业务配置管理 admin API。
	settingsRepo := settings.NewPGRepository(database)
	settingsHandler := admin.NewSettingsHandler(settingsRepo, auditRepo, cfg, healthCollector)
	admin.RegisterSettingsRoutes(mux, settingsHandler, repo, authCfg)

	// 全局 middleware 包装：RequestID → Logging → mux
	// 引入动机：design/06-IMPLEMENTATION.md Phase 1 要求 middleware (auth, request_id, slog)。
	// RequestID 必须在最外层，确保 Logging 能从 context 获取 request ID，
	// 且所有路由（含公开和认证端点）都有 correlation ID。
	// 认证 middleware（AuthMiddleware → RequireAuth → RequireCSRF → permission → handler）
	// 已在各自 RegisterRoutes 中按路由挂载，此处全局包装不破坏现有认证链。
	handler := httpmw.RequestIDMiddleware(httpmw.LoggingMiddleware(mux))

	// 启动 HTTP 服务器
	httpServer := &http.Server{
		Addr:    cfg.HTTPAddr,
		Handler: handler,
	}

	go func() {
		slog.Info("HTTP 服务器启动", "addr", cfg.HTTPAddr)
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("HTTP 服务器错误", "error", err)
		}
	}()

	// 创建 server 级可取消 context，基于传入 ctx 派生。
	// 引入动机：design/05-OPERATIONS.md §Graceful Shutdown 要求 SIGTERM 不再无限 Background 运行，
	// 停止接受请求并有界取消/等待 worker。此 context 在收到关闭信号或传入 ctx 取消时被取消，
	// 传递给 worker 和 scheduler 使它们安全退出。
	// Phase 6 修复：原实现使用 context.Background() 派生 serverCtx，导致测试中传入的 ctx
	// 取消无法传播到 serverCtx，worker/scheduler/信号等待不受测试控制，造成全量测试挂起。
	// 现在改为基于传入 ctx 派生，确保 ctx 取消能触发同一优雅关闭路径。
	serverCtx, serverCancel := context.WithCancel(ctx)

	// 启动 Job Worker（如果启用）
	// 引入动机：design/05-OPERATIONS.md §Background Jobs 要求 PG-backed queue + worker。
	// C5：index job 通过 embedding provider 生成向量；C7/C8：rebuild/cleanup 通过 profile repo 获取配置。
	// evaluate_profile/optimize_profile job 需要 eval repo、result repo、profile repo 和搜索管线。
	// Phase 6 修复：worker 使用 serverCtx 而非 context.Background，使 SIGTERM 能有界取消 worker。
	var jobWorker *job.Worker
	if cfg.JobWorkerEnabled {
		// 恢复 stale running jobs（worker 崩溃后遗留的 running 状态 job）
		// 引入动机：design/05-OPERATIONS.md §Background Jobs 要求支持故障恢复。
		// worker 崩溃或被 SIGKILL 后，其 claim 的 job 会永久停留在 running 状态。
		// 启动时恢复这些 stale job，使其重新被 worker claim 执行。
		recoveredCount, recoverErr := jobRepo.RecoverStaleJobs(ctx, job.DefaultStaleJobTimeout)
		if recoverErr != nil {
			slog.Warn("恢复 stale running job 失败", "error", recoverErr)
		} else if recoveredCount > 0 {
			slog.Info("已恢复 stale running job", "count", recoveredCount)
		}
		// 将 embedding.Provider 适配为 job 所需的函数签名。
		//
		// 引入动机：job worker 需要从 Registry 动态获取 Provider，支持热更新。
		// 使用 Registry 函数引用而非启动期静态实例，使 Provider 保存后 job 立即使用新实例。
		//
		// 修复说明：原实现仅在启动时 Provider 已配置才创建闭包，导致启动时未配置但
		// 后续通过 Web API 热更新 Provider 后 job 仍无法使用（闭包为 nil）。
		// 现在无条件创建闭包，每次调用从 Registry 动态获取当前 Provider 实例，
		// 无论 Provider 何时被配置，job 都能立即使用。
		embEmbed := func(ctx context.Context, texts []string) ([][]float32, error) {
			p := providerRegistry.GetEmbeddingProvider()
			if p == nil {
				return nil, fmt.Errorf("embedding provider 未配置")
			}
			return p.Embed(ctx, texts)
		}
		// 仅判断 provider 是否已配置；正式 Embed 调用负责发现网络/API 故障，
		// 避免 index job 在 Embed 前额外发起一次 health embedding 请求。
		embConfigured := func(ctx context.Context) bool {
			return providerRegistry.GetEmbeddingProvider() != nil
		}
		// 将 profile.PGRepository 适配为 job.ProfileRepo
		jobProfileRepo := &profileRepoAdapter{repo: profileRepo}
		// 第 6 个实参注入 jobRepo（*job.PGRepository，已满足 job.JobEnqueuer）。
		// 引入动机：index_document 兜底路径一旦发现 alias 指向索引的 embedding 维度与
		// profile 不一致，写入必被 ES 拒绝，需要自动投递一次 rebuild_index，
		// 并通过查询队列避免重复入队。
		baseJobHandler := job.NewIndexJobHandler(database, esClient, embEmbed, embConfigured, jobProfileRepo, jobRepo, docRepo)

		// 构建带评测能力的 job handler
		profileRepoExt := &profileRepoExtAdapter{repo: profileRepo}
		evalJobHandler := job.NewEvalJobHandler(baseJobHandler, evalRepo, evalResultRepo, profileRepoExt, pipe)

		// 使用固定 UUID 作为 worker ID，因为 jobs.locked_by 列是 UUID 类型。
		// 引入动机：修复 worker-1 字符串无法写入 UUID 列的问题。
		// 多实例部署时各实例应使用不同的 UUID，此处使用基于 hostname 的确定性 UUID。
		workerID := uuid.New().String()
		jobWorker = job.NewWorker(jobRepo, evalJobHandler, workerID)
		go jobWorker.Start(serverCtx, time.Duration(cfg.JobWorkerPollInterval)*time.Second)
		slog.Info("Job Worker 已启动（含评测/调优支持）")
	}

	// 启动维护调度器（如果启用）
	// 引入动机：design/05-OPERATIONS.md §Search Maintenance 要求周期性执行维护任务。
	// 调度器使用 PG 唯一约束实现分布式去重，内存 ticker 仅作为触发器。
	var sched *scheduler.Scheduler
	if cfg.SchedulerEnabled {
		scheduleRepo := scheduler.NewPGScheduleRepo(database)
		sched = scheduler.NewScheduler(scheduleRepo, scheduler.AllTasks(), time.Duration(cfg.SchedulerTickInterval)*time.Second)
		go sched.Start(serverCtx)
		slog.Info("维护调度器已启动")
	}

	// 等待关闭信号或传入 ctx 取消。
	// Phase 6 修复：signal.NotifyContext 基于传入 ctx 而非 context.Background()，
	// 确保测试中 ctx 取消能触发相同的优雅关闭路径，而非无限阻塞在信号等待。
	sigCtx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	<-sigCtx.Done()
	slog.Info("收到关闭信号，开始优雅关闭")

	// 1. 停止接受新 HTTP 请求（优雅关闭 HTTP 服务器）
	// 引入动机：design/05-OPERATIONS.md 要求先停止接受新请求，再取消 worker。
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer shutdownCancel()

	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		slog.Error("HTTP 服务器关闭错误", "error", err)
	}

	// 2. 取消 server context，通知 worker 和 scheduler 停止
	// 引入动机：worker 和 scheduler 使用 serverCtx，取消后它们会安全退出 poll loop。
	serverCancel()

	// 3. 停止 Job Worker（等待退出或超时）
	// 引入动机：确保进行中的任务安全完成，不丢失持久化任务或泄漏 claim。
	// Worker.Start 已在 serverCtx.Done() 时返回，Stop 关闭 stopCh 作为备用信号并等待 done。
	// 使用有界等待避免 job handler 无限阻塞导致进程无法退出。
	if jobWorker != nil {
		workerStopped := make(chan struct{})
		go func() {
			jobWorker.Stop()
			close(workerStopped)
		}()
		select {
		case <-workerStopped:
			// Worker 已安全退出
		case <-time.After(cfg.ShutdownTimeout):
			slog.Error("Job Worker 停止超时，可能仍有任务在执行")
		}
	}

	// 4. 停止维护调度器（等待退出）
	// 引入动机：scheduler 使用 serverCtx，取消后已退出。Stop 确保 done channel 关闭。
	if sched != nil {
		sched.Stop()
	}

	slog.Info("优雅关闭完成")
	return nil
}

// initLogger 根据日志级别初始化全局 slog logger。
// 日志级别已在 config.Load() 中验证，此处不会出现非法值。
func initLogger(level string) {
	var sl slog.Level
	switch level {
	case "debug":
		sl = slog.LevelDebug
	case "info":
		sl = slog.LevelInfo
	case "warn":
		sl = slog.LevelWarn
	case "error":
		sl = slog.LevelError
	default:
		// 配置加载阶段已验证合法性，不应到达此处
		slog.Error("内部错误：未知的日志级别，降级为 info", "level", level)
		sl = slog.LevelInfo
	}

	handler := slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: sl})
	slog.SetDefault(slog.New(handler))
}

// findMigrationsDir 定位迁移文件目录。
// 优先使用环境变量 MIGRATIONS_DIR，其次使用相对于可执行文件的 ../migrations。
func findMigrationsDir() string {
	if dir := os.Getenv("MIGRATIONS_DIR"); dir != "" {
		return dir
	}

	exe, err := os.Executable()
	if err == nil {
		candidate := filepath.Join(filepath.Dir(exe), "..", "migrations")
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
	}

	// 回退到工作目录下的 migrations
	return "migrations"
}

// profileRepoAdapter 将 profile.PGRepository 适配为 job.ProfileRepo 接口。
// 引入动机：job 包定义了独立的 ProfileRepo 接口以避免循环依赖，
// main.go 作为组装层负责将 profile.PGRepository 适配为 job.ProfileRepo。

// specialDocsInitAdapter 将 document.PGRepository 适配为 workspace.SpecialDocumentsInitializer。
// 引入动机：C1 要求 CreateSpecialDocuments 接收 TxJobEnqueuer 参数，
// 但 document.TxJobEnqueuer 和 workspace.TxJobEnqueuer 是两个独立定义的命名接口，
// Go 要求方法签名中的命名接口类型完全匹配，因此需要适配器桥接。
// 任何实现 workspace.TxJobEnqueuer 的值也实现 document.TxJobEnqueuer（结构化接口）。
type specialDocsInitAdapter struct {
	docRepo *document.PGRepository
}

func (a *specialDocsInitAdapter) CreateSpecialDocuments(ctx context.Context, tx *sql.Tx, workspaceID, createdBy string, jobEnq workspace.TxJobEnqueuer) error {
	return a.docRepo.CreateSpecialDocuments(ctx, tx, workspaceID, createdBy, jobEnq)
}

type profileRepoAdapter struct {
	repo *profile.PGRepository
}

func (a *profileRepoAdapter) GetActiveProfile(ctx context.Context) (*job.ProfileForJob, error) {
	p, err := a.repo.GetActiveProfile(ctx)
	if err != nil {
		return nil, err
	}
	return &job.ProfileForJob{
		ID:                        p.ID,
		ESIndexName:               p.ESIndexName,
		ChunkTargetSize:           p.ChunkTargetSize,
		ChunkOverlap:              p.ChunkOverlap,
		EmbeddingDimensions:       p.EmbeddingDimensions,
		EmbeddingQueryInstruction: p.EmbeddingQueryInstruction,
		EmbeddingDocInstruction:   p.EmbeddingDocInstruction,
		// Phase 6 修复：传递 Analyzer 字段，使 rebuild_index 能创建正确的 ES mapping。
		Analyzer: p.Analyzer,
	}, nil
}

// UpdateProfileESIndex 将 rebuild 维度自愈后产生的新索引名回写到 profile。
// 引入动机：rebuild 在 embedding 维度不一致时会创建新索引并切换 alias，
// 组装层需把该能力桥接给 job 层，否则 profile 记录会与 alias 实际指向不一致。
func (a *profileRepoAdapter) UpdateProfileESIndex(ctx context.Context, id, indexName string) error {
	return a.repo.UpdateProfileESIndex(ctx, id, indexName)
}

// profileRepoExtAdapter 将 profile.PGRepository 适配为 job.ProfileRepoExtended 接口。
// 引入动机：optimize_profile job 需要完整的 profile 操作（获取/创建候选/激活/记录状态），
// main.go 作为组装层负责将 profile.PGRepository 适配为 job.ProfileRepoExtended。
type profileRepoExtAdapter struct {
	repo *profile.PGRepository
}

func (a *profileRepoExtAdapter) GetActiveProfile(ctx context.Context) (*job.ProfileForJob, error) {
	return (&profileRepoAdapter{repo: a.repo}).GetActiveProfile(ctx)
}

// UpdateProfileESIndex 将 rebuild 维度自愈后产生的新索引名回写到 profile。
// 引入动机：job.ProfileRepoExtended 内嵌了 job.ProfileRepo，因此该适配器必须一并实现
// UpdateProfileESIndex，否则无法作为 job.ProfileRepoExtended 注入 NewEvalJobHandler；
// 同时 optimize_profile 触发的 rebuild 若回写索引名失败，候选 profile 的 es_index_name
// 会与 alias 实际指向不一致，故必须如实转调底层仓储而不是静默丢弃。
func (a *profileRepoExtAdapter) UpdateProfileESIndex(ctx context.Context, id, indexName string) error {
	return a.repo.UpdateProfileESIndex(ctx, id, indexName)
}

func (a *profileRepoExtAdapter) GetProfileByID(ctx context.Context, id string) (*job.ProfileForJobExtended, error) {
	p, err := a.repo.GetProfileByID(ctx, id)
	if err != nil {
		return nil, err
	}
	return &job.ProfileForJobExtended{
		ID:                        p.ID,
		Name:                      p.Name,
		Version:                   p.Version,
		ESIndexName:               p.ESIndexName,
		ChunkTargetSize:           p.ChunkTargetSize,
		ChunkOverlap:              p.ChunkOverlap,
		EmbeddingProvider:         p.EmbeddingProvider,
		EmbeddingModel:            p.EmbeddingModel,
		EmbeddingDimensions:       p.EmbeddingDimensions,
		EmbeddingQueryInstruction: p.EmbeddingQueryInstruction,
		EmbeddingDocInstruction:   p.EmbeddingDocInstruction,
		Analyzer:                  p.Analyzer,
		RerankerProvider:          p.RerankerProvider,
		RerankerModel:             p.RerankerModel,
		TitleBoost:                p.TitleBoost,
		HeadingBoost:              p.HeadingBoost,
		PathBoost:                 p.PathBoost,
		TagsBoost:                 p.TagsBoost,
		BodyBoost:                 p.BodyBoost,
		LexicalTopK:               p.LexicalTopK,
		VectorTopK:                p.VectorTopK,
		RRFK:                      p.RRFK,
		RerankerCandidateCount:    p.RerankerCandidateCount,
		RerankerFinalCount:        p.RerankerFinalCount,
		MaxChunksPerDocument:      p.MaxChunksPerDocument,
		MergeAdjacentChunks:       p.MergeAdjacentChunks,
		MaxP95LatencyMs:           p.MaxP95LatencyMs,
		MaxRerankerCostPerQuery:   p.MaxRerankerCostPerQuery,
		CreatedBy:                 p.CreatedBy,
	}, nil
}

func (a *profileRepoExtAdapter) CreateCandidateProfile(ctx context.Context, base *job.ProfileForJobExtended, changes job.ParameterChanges) (*job.ProfileForJobExtended, error) {
	// 基于 base profile 创建新 profile，继承真实 active 配置的 provider/model/analyzer。
	// 引入动机：design/01-SEARCH.md §Auto Tuning Level 1 仅允许调整
	// field boost/top_k/RRF/candidate count/diversity，
	// embedding/reranker provider/model/analyzer 不可变，必须从 base 继承。
	// CreatedBy 继承 base profile 的创建者，保证审计可追溯，不使用伪造 UUID。
	input := &profile.CreateProfileInput{
		Name:                      base.Name,
		EmbeddingProvider:         base.EmbeddingProvider,
		EmbeddingModel:            base.EmbeddingModel,
		EmbeddingDimensions:       base.EmbeddingDimensions,
		EmbeddingQueryInstruction: base.EmbeddingQueryInstruction,
		EmbeddingDocInstruction:   base.EmbeddingDocInstruction,
		ChunkTargetSize:           base.ChunkTargetSize,
		ChunkOverlap:              base.ChunkOverlap,
		TitleBoost:                base.TitleBoost,
		HeadingBoost:              base.HeadingBoost,
		PathBoost:                 base.PathBoost,
		TagsBoost:                 base.TagsBoost,
		BodyBoost:                 base.BodyBoost,
		Analyzer:                  base.Analyzer,
		LexicalTopK:               base.LexicalTopK,
		VectorTopK:                base.VectorTopK,
		RRFK:                      base.RRFK,
		RerankerProvider:          base.RerankerProvider,
		RerankerModel:             base.RerankerModel,
		RerankerCandidateCount:    base.RerankerCandidateCount,
		RerankerFinalCount:        base.RerankerFinalCount,
		MaxChunksPerDocument:      base.MaxChunksPerDocument,
		MergeAdjacentChunks:       base.MergeAdjacentChunks,
		MaxP95LatencyMs:           base.MaxP95LatencyMs,
		MaxRerankerCostPerQuery:   base.MaxRerankerCostPerQuery,
		CreatedBy:                 base.CreatedBy,
	}

	// 应用 Level 1 参数变更
	if changes.TitleBoost != nil {
		input.TitleBoost = *changes.TitleBoost
	}
	if changes.HeadingBoost != nil {
		input.HeadingBoost = *changes.HeadingBoost
	}
	if changes.PathBoost != nil {
		input.PathBoost = *changes.PathBoost
	}
	if changes.TagsBoost != nil {
		input.TagsBoost = *changes.TagsBoost
	}
	if changes.BodyBoost != nil {
		input.BodyBoost = *changes.BodyBoost
	}
	if changes.LexicalTopK != nil {
		input.LexicalTopK = *changes.LexicalTopK
	}
	if changes.VectorTopK != nil {
		input.VectorTopK = *changes.VectorTopK
	}
	if changes.RRFK != nil {
		input.RRFK = *changes.RRFK
	}
	if changes.RerankerCandidateCount != nil {
		input.RerankerCandidateCount = *changes.RerankerCandidateCount
	}
	if changes.MaxChunksPerDocument != nil {
		input.MaxChunksPerDocument = *changes.MaxChunksPerDocument
	}
	if changes.MergeAdjacentChunks != nil {
		input.MergeAdjacentChunks = *changes.MergeAdjacentChunks
	}

	p, err := a.repo.CreateProfile(ctx, input)
	if err != nil {
		return nil, err
	}
	return &job.ProfileForJobExtended{
		ID:                        p.ID,
		Name:                      p.Name,
		Version:                   p.Version,
		ESIndexName:               p.ESIndexName,
		ChunkTargetSize:           p.ChunkTargetSize,
		ChunkOverlap:              p.ChunkOverlap,
		EmbeddingProvider:         p.EmbeddingProvider,
		EmbeddingModel:            p.EmbeddingModel,
		EmbeddingDimensions:       p.EmbeddingDimensions,
		EmbeddingQueryInstruction: p.EmbeddingQueryInstruction,
		EmbeddingDocInstruction:   p.EmbeddingDocInstruction,
		Analyzer:                  p.Analyzer,
		RerankerProvider:          p.RerankerProvider,
		RerankerModel:             p.RerankerModel,
		TitleBoost:                p.TitleBoost,
		HeadingBoost:              p.HeadingBoost,
		PathBoost:                 p.PathBoost,
		TagsBoost:                 p.TagsBoost,
		BodyBoost:                 p.BodyBoost,
		LexicalTopK:               p.LexicalTopK,
		VectorTopK:                p.VectorTopK,
		RRFK:                      p.RRFK,
		RerankerCandidateCount:    p.RerankerCandidateCount,
		RerankerFinalCount:        p.RerankerFinalCount,
		MaxChunksPerDocument:      p.MaxChunksPerDocument,
		MergeAdjacentChunks:       p.MergeAdjacentChunks,
		MaxP95LatencyMs:           p.MaxP95LatencyMs,
		MaxRerankerCostPerQuery:   p.MaxRerankerCostPerQuery,
		CreatedBy:                 p.CreatedBy,
	}, nil
}

func (a *profileRepoExtAdapter) ActivateProfile(ctx context.Context, id string) error {
	return a.repo.ActivateProfile(ctx, id)
}

func (a *profileRepoExtAdapter) CreateCandidateRecord(ctx context.Context, baseProfileID, candidateProfileID string, tuningLevel int, parameterChanges json.RawMessage) error {
	return a.repo.CreateCandidate(ctx, &profile.CandidateRecord{
		BaseProfileID:      baseProfileID,
		CandidateProfileID: candidateProfileID,
		TuningLevel:        tuningLevel,
		ParameterChanges:   parameterChanges,
		Status:             "testing",
	})
}

func (a *profileRepoExtAdapter) UpdateCandidateStatus(ctx context.Context, id string, status string, gatePassed bool, gateDetails json.RawMessage, evaluationResult json.RawMessage) error {
	return a.repo.UpdateCandidateStatus(ctx, id, status, gatePassed, gateDetails, evaluationResult)
}
