package cmd

import (
	"net/http"
	"os"
	"strconv"
	"strings"

	"github.com/alecthomas/kingpin/v2"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/pstrobl96/prusa_exporter/config"
	prusalink "github.com/pstrobl96/prusa_exporter/prusalink/buddy"
	udp "github.com/pstrobl96/prusa_exporter/udp"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
)

var (
	configFile             = kingpin.Flag("config.file", "Configuration file for prusa_exporter.").Default("./prusa.yml").ExistingFile()
	metricsPath            = kingpin.Flag("exporter.metrics-path", "Path where to expose Prusa Link metrics.").Default("/metrics/prusalink").String()
	udpMetricsPath         = kingpin.Flag("exporter.udp-metrics-path", "Path where to expose udp metrics.").Default("/metrics/udp").String()
	metricsPort            = kingpin.Flag("exporter.metrics-port", "Port where to expose metrics.").Default("10009").Int()
	ipOverride             = kingpin.Flag("exporter.ip-override", "Override the IP address of the server with this value.").Default("").String()
	prusaLinkScrapeTimeout = kingpin.Flag("prusalink.scrape-timeout", "Timeout in seconds to scrape prusalink metrics.").Default("10").Int()
	logLevel               = kingpin.Flag("log.level", "Log level for zerolog.").Default("info").String()
	syslogListenAddress    = kingpin.Flag("listen-address", "Address where to expose port for gathering metrics. - format <address>:<port>").Default("0.0.0.0:8514").String()
	udpPrefix              = kingpin.Flag("prefix", "Prefix for udp metrics").Default("prusa_").String()
	udpRegistry            = prometheus.NewRegistry()
)

// handleJobImage handles requests for job images by serial number
func handleJobImage(w http.ResponseWriter, r *http.Request, config config.Config) {
	// Extract serial number from path: /{serial}/jobimage.png
	path := strings.TrimPrefix(r.URL.Path, "/")
	parts := strings.Split(path, "/")

	if len(parts) != 2 || parts[1] != "jobimage.png" {
		http.NotFound(w, r)
		return
	}

	serial := parts[0]
	if serial == "" {
		http.NotFound(w, r)
		return
	}

	// Find printer by serial number
	printer, err := prusalink.FindPrinterBySerial(serial, config.Printers)
	if err != nil {
		log.Error().Msgf("Printer with serial %s not found: %v", serial, err)
		http.NotFound(w, r)
		return
	}

	// Get current job information
	job, err := prusalink.GetJob(*printer)
	if err != nil {
		log.Error().Msgf("Failed to get job for printer %s: %v", serial, err)
		http.Error(w, "Failed to get job information", http.StatusInternalServerError)
		return
	}

	// Check if there's an active job with a file
	if job.Job.File.Path == "" {
		log.Debug().Msgf("No active job or job file for printer %s", serial)
		http.NotFound(w, r)
		return
	}

	// Get the job image as PNG
	imageData, err := prusalink.GetJobImagePNG(*printer, job.Job.File.Path)
	if err != nil {
		log.Error().Msgf("Failed to get job image for printer %s: %v", serial, err)
		http.Error(w, "Failed to get job image", http.StatusInternalServerError)
		return
	}

	// Set appropriate headers and serve the image
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Content-Length", strconv.Itoa(len(imageData)))
	w.Header().Set("Cache-Control", "public, max-age=30") // Cache for 30 seconds

	_, err = w.Write(imageData)
	if err != nil {
		log.Error().Msgf("Failed to write image data: %v", err)
	}
}

// Run function to start the exporter
func Run() {
	kingpin.Parse()
	log.Info().Msg("Prusa exporter starting")

	if *udpMetricsPath == *metricsPath {
		log.Panic().Msg("udp_metrics_path must be different from metrics_path")
	}

	if _, err := os.Stat(*configFile); os.IsNotExist(err) {
		log.Panic().Msg("Configuration file does not exist: " + *configFile)
	}

	log.Info().Msg("Loading configuration file: " + *configFile)

	config, err := config.LoadConfig(*configFile, *prusaLinkScrapeTimeout, *ipOverride)

	if err != nil {
		log.Panic().Msg("Error loading configuration file " + err.Error())
	}

	logLevel, err := zerolog.ParseLevel(*logLevel)

	if err != nil {
		logLevel = zerolog.InfoLevel // default log level
	}
	zerolog.SetGlobalLevel(logLevel)

	var collectors []prometheus.Collector

	log.Info().Msg("PrusaLink metrics enabled!")
	collectors = append(collectors, prusalink.NewCollector(config))
	prusalink.EnableUDPmetrics(config.Printers)

	// starting syslog server

	log.Info().Msg("Syslog server starting at: " + *syslogListenAddress)
	go udp.MetricsListener(*syslogListenAddress, *udpPrefix)
	log.Info().Msg("Syslog server ready to receive metrics")

	// registering the prometheus metrics

	prometheus.MustRegister(collectors...)
	log.Info().Msg("Metrics registered")
	http.Handle(*metricsPath, promhttp.Handler())
	log.Info().Msg("PrusaLink metrics initialized")

	udp.Init(udpRegistry)

	http.Handle(*udpMetricsPath, promhttp.HandlerFor(udpRegistry, promhttp.HandlerOpts{
		Registry: udpRegistry,
	}))
	log.Info().Msg("UDP metrics initialized")

	log.Info().Msg("Listening at port: " + strconv.Itoa(*metricsPort))

	// Handle job image requests and root path
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		// Check if the path matches the job image pattern: /{serial}/jobimage.png
		path := r.URL.Path
		if len(path) > 1 && strings.HasSuffix(path, "/jobimage.png") {
			handleJobImage(w, r, config)
			return
		}

		// Default root handler
		html := `<html>
    <head><title>prusa_exporter 2.0.0-alpha2</title></head>
    <body>
    <h1>prusa_exporter</h1>
	<p>Syslog server running at - <b>` + *syslogListenAddress + `</b></p>
    <p><a href="` + *metricsPath + `">PrusaLink metrics</a></p>
	<p><a href="` + *udpMetricsPath + `">UDP Metrics</a></p>
	<h2>Job Images</h2>
	<p>Access job images via: <code>/{printer-serial}/jobimage.png</code></p>
	<p>Example: <code>/12345-4235324534563453/jobimage.png</code></p>
	</body>
    </html>`
		w.Write([]byte(html))
	})

	log.Fatal().Msg(http.ListenAndServe(":"+strconv.Itoa(*metricsPort), nil).Error())

}
