package main

import (
	"log/slog"
	"os"

	"github.com/guigui42/scan-hub/internal/config"
	"github.com/guigui42/scan-hub/internal/scanner"
	"github.com/guigui42/scan-hub/internal/scanner/escldrv"
	"github.com/guigui42/scan-hub/internal/scanner/scansnapdrv"
	"github.com/guigui42/scan-hub/internal/scannerconfig"
)

// loadScanners reads SCANHUB_SCANNERS_FILE. The returned ScanSnap driver must
// be closed after the devices have stopped.
func loadScanners(cfg config.Config, log *slog.Logger) ([]scanner.Device, *scansnapdrv.Driver, error) {
	ss := scansnapdrv.New(scansnapdrv.Options{DataDir: cfg.DataDir, Logger: log, LookupEnv: os.LookupEnv})
	if cfg.ScannersFile == "" {
		return nil, ss, nil
	}
	devs, err := scannerconfig.Load(cfg.ScannersFile, map[string]scannerconfig.Driver{
		scansnapdrv.Type: ss,
		escldrv.Type:     escldrv.New(escldrv.Options{Logger: log}),
	})
	if err != nil {
		ss.Close()
		return nil, nil, err
	}
	return devs, ss, nil
}

func scannerIDs(devs []scanner.Device) []string {
	ids := make([]string, len(devs))
	for i, d := range devs {
		ids[i] = d.ID()
	}
	return ids
}
