// PLC Monitoring — Version 01
//
// One process runs independent modules that talk through a small in-process
// sample bus (Go channels); main only creates, connects and stops them:
//
//	PLC → acquisition ──► bus ──► storage  (PostgreSQL history)
//	                          ├─► live data  (dashboard, PV detail, pushed to browsers)
//	                          └─► alarms     (alarm engine)           [later stage]
//
// The acquisition loop never waits for a consumer: every consumer has its own
// bounded queue and an explicit overload policy (see internal/bus).
//
// Run from the project folder:  go run ./cmd/server
package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"time"

	"github.com/gin-gonic/gin"

	"PLC_Monitoring/internal/acquisition"
	"PLC_Monitoring/internal/bus"
	"PLC_Monitoring/internal/config"
	"PLC_Monitoring/internal/live"
	"PLC_Monitoring/internal/storage"
	"PLC_Monitoring/internal/store"
	"PLC_Monitoring/internal/web"
)

// Queue of the storage consumer on the bus. The storage module drains it
// continuously into its own bounded memory buffer, so this only has to absorb
// short stalls (50 000 samples are minutes of normal traffic).
const storageQueue = 50_000

// Queue of the live-data consumer: it only keeps the newest value per PV, so it
// drains fast; if it ever falls behind, the oldest queued samples are dropped
// (and counted) because a newer value for the same PV follows anyway.
// maxLiveClients bounds the browsers that may hold a pushed-updates connection.
const (
	liveQueue      = 10_000
	maxLiveClients = 200
)

func main() {
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	cfg := config.Load()

	// Ctrl+C cancels ctx → acquisition and the HTTP server stop. The storage
	// module is stopped separately, after acquisition, so it can write what is queued.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	db, err := store.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		log.Fatalf("PostgreSQL: %v", err) // nothing works without the DB
	}
	defer db.Close()
	log.Println("connected to PostgreSQL")

	// --- sample bus and its consumers (consumers subscribe BEFORE acquisition starts) ---
	sampleBus := bus.New(nil)
	storageModule := storage.New(db, sampleBus.Subscribe("storage", storageQueue, bus.DropOldest), storage.Options{})
	storageDone := make(chan struct{})
	go func() {
		defer close(storageDone)
		storageModule.Run()
	}()

	liveModule := live.New(db.LatestValues, sampleBus.Subscribe("live", liveQueue, bus.DropOldest), maxLiveClients)
	// Initial values: PV configuration and last known values, read once. If the
	// database cannot answer now, the module fills from the first samples.
	if err := liveModule.Refresh(ctx); err != nil {
		log.Printf("live: initial values not loaded: %v", err)
	}
	liveDone := make(chan struct{})
	go func() {
		defer close(liveDone)
		liveModule.Run()
	}()

	// --- acquisition ---
	// PLCs are NOT connected here. Each PLC worker connects (and reconnects)
	// on its own, so an offline PLC never stops the application.
	acqDone := make(chan struct{})
	onConfigChange := func() { go liveModule.Refresh(context.Background()) } // called by the web UI after a config save
	var acqStats func() []acquisition.WorkerStats
	if cfg.AcquireEnabled {
		mgr := acquisition.NewManager(db, cfg.ConfigReload, sampleBus)
		onConfigChange = func() {
			mgr.Reload()
			go liveModule.Refresh(context.Background())
		}
		acqStats = mgr.Stats
		go func() {
			defer close(acqDone)
			mgr.Run(ctx)
		}()
	} else {
		close(acqDone)
		log.Println("acquisition disabled (ACQUISITION=off)")
	}

	// --- web ---
	gin.SetMode(gin.ReleaseMode)
	httpSrv := &http.Server{Addr: cfg.HTTPAddr, Handler: web.NewRouter(db, onConfigChange, web.Options{
		CookieSecure:    cfg.CookieSecure,
		SameSiteStrict:  cfg.CookieSameSite == "strict",
		SessionLifetime: cfg.SessionHours,
		Live:            liveModule,
		// Health of the sample pipeline: queue sizes and every drop, for operators.
		Pipeline: func() any {
			out := map[string]any{"bus": sampleBus.Stats(), "storage": storageModule.Stats(), "live": liveModule.Stats()}
			if acqStats != nil {
				out["acquisition"] = acqStats()
			}
			return out
		},
	})}
	go func() {
		log.Printf("web UI on http://localhost%s", cfg.HTTPAddr)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("HTTP server: %v", err)
		}
	}()

	<-ctx.Done()
	log.Println("shutting down…")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	httpSrv.Shutdown(shutdownCtx)

	// Order matters: stop producing, then let the consumers finish what is queued.
	<-acqDone
	sampleBus.Close()
	<-storageDone
	<-liveDone
}
