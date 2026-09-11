package core

import (
	"log"
	"strconv"
	"time"
)

type LauncherConfig struct {
	IsSetupDone       bool
	UpdateDomainsFunc func() error
	RunFullSyncFunc   func() error
	GetSettingFunc    func(key, defaultVal string) string
}

var schedulerStop chan struct{}

// StopScheduler signals the background scheduler goroutines to exit.
func StopScheduler() {
	if schedulerStop != nil {
		close(schedulerStop)
	}
}

func backgroundSyncWrapper(config LauncherConfig) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("💥 Panic catturato durante la sequenza di avvio: %v", r)
		}
	}()

	if err := config.UpdateDomainsFunc(); err != nil {
		log.Printf("Errore aggiornamento domini: %v", err)
	}

	if config.RunFullSyncFunc != nil {
		if err := config.RunFullSyncFunc(); err != nil {
			log.Printf("Errore sincronizzazione catalogo: %v", err)
		}
	}

}

// runScheduled loops forever, running task every N hours (re-read from config each cycle).
func runScheduled(name string, task func() error, intervalKey string, defaultHours int, getSetting func(string, string) string, stop <-chan struct{}) {
	for {
		hours := defaultHours
		if getSetting != nil {
			if v, err := strconv.Atoi(getSetting(intervalKey, "")); err == nil && v > 0 {
				hours = v
			}
		}
		select {
		case <-stop:
			return
		case <-time.After(time.Duration(hours) * time.Hour):
		}
		log.Printf("⏰ [Scheduler:%s] avvio schedulato (ogni %dh)…", name, hours)
		// A panic in one scheduled run must not kill this loop forever — that
		// job would then silently never run again until the process restarts.
		runScheduledTaskOnce(name, task)
	}
}

func runScheduledTaskOnce(name string, task func() error) {
	defer Guard("core/scheduled-task:" + name)
	if err := task(); err != nil {
		log.Printf("⚠️ [Scheduler:%s] %v", name, err)
	}
}

// StartAppEngine avvia il motore e i job di manutenzione ricorrenti.
func StartAppEngine(config LauncherConfig) bool {
	if !config.IsSetupDone {
		log.Println("🛑 [Primo Avvio] Configurazione mancante. Motore in standby.")
		log.Println("👉 Apri il browser e vai su: http://127.0.0.1:8000/setup per iniziare.")
		return false
	}

	log.Println("🚀 Avvio del motore e delle sincronizzazioni in background...")

	schedulerStop = make(chan struct{})
	stop := schedulerStop

	go backgroundSyncWrapper(config)

	// Catalog sync is now handled per-plugin by the PluginManager's runSyncScheduler.
	go runScheduled("domains", config.UpdateDomainsFunc, "domain_interval_hours", 6, config.GetSettingFunc, stop)

	return true
}
