package main

import (
	"context"
	"flag"
	"log"
	"math/rand"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/VictoriaMetrics/fastcache"
	"github.com/gorilla/handlers"
	"github.com/gorilla/mux"

	"github.com/doingodswork/deflix-stremio/pkg/imdb2torrent"
	"github.com/doingodswork/deflix-stremio/pkg/realdebrid"
	"github.com/doingodswork/deflix-stremio/pkg/stremio"
)

const (
	version = "0.1.0"
)

// Flags
var (
	bindAddr      = *flag.String("bindAddr", "localhost", "Local interface address to bind to. \"0.0.0.0\" binds to all interfaces.")
	port          = *flag.Int("port", 8080, "Port to listen on")
	streamURLaddr = *flag.String("streamURLaddr", "http://localhost:8080", "Address to be used in a stream URL that's delivered to Stremio and later used to redirect to RealDebrid")
	cachePath     = *flag.String("cachePath", "", "Path for loading a persisted cache on startup and persisting the current cache in regular intervals. An empty value will lead to `os.UserCacheDir()+\"/deflix-stremio/\"`")
	// 128*1024*1024 are 128 MB
	// We split these on 4 caches à 32 MB
	// Note: fastcache uses 32 MB as minimum, that's why we use `4*32 MB = 128 MB` as minimum.
	cacheMaxBytes = *flag.Int("cacheMaxBytes", 128*1024*1024, "Max number of bytes to be used for the in-memory cache. Default (and minimum!) is 128 MB.")
)

var manifest = stremio.Manifest{
	ID:          "tv.deflix.stremio",
	Name:        "Deflix - Debrid flicks",
	Description: "Automatically turns torrents into debrid/cached streams, for high speed and no seeding. Currently supported providers: real-debrid.com (more coming soon™).",
	Version:     version,

	ResourceItems: []stremio.ResourceItem{
		stremio.ResourceItem{
			Name:  "stream",
			Types: []string{"movie"},
			// Not required as long as we define them globally in the manifest
			//IDprefixes: []string{"tt"},
		},
	},
	Types: []string{"movie"},
	// An empty slice is required for serializing to a JSON that Stremio expects
	Catalogs: []stremio.CatalogItem{},

	IDprefixes: []string{"tt"},
	// Must use www.deflix.tv instead of just deflix.tv because GitHub takes care of redirecting non-www to www and this leads to HTTPS certificate issues.
	Background: "https://www.deflix.tv/images/Logo-1024px.png",
	Logo:       "https://www.deflix.tv/images/Logo-250px.png",
}

// In-memory cache, which is filled from a file on startup and persisted to a file in regular intervals.
// Use four different caches so that for example a high churn (new entries pushing out old ones) in the torrent cache doesn't lead to important redirect entries to be lost before used by the user.
var (
	torrentCache      *fastcache.Cache
	tokenCache        *fastcache.Cache
	availabilityCache *fastcache.Cache
	redirectCache     *fastcache.Cache
)

func init() {
	// Timeout for global default HTTP client (for when using `http.Get()`)
	http.DefaultClient.Timeout = 5 * time.Second

	// Make predicting "random" numbers harder
	rand.NewSource(time.Now().UnixNano())
}

func main() {
	flag.Parse()

	// Load or create caches

	if cachePath == "" {
		userCacheDir, err := os.UserCacheDir()
		if err != nil {
			log.Fatal("Couldn't determine user cache directory via `os.UserCacheDir()`:", err)
		}
		cachePath = userCacheDir + "/deflix-stremio"
	} else {
		cachePath = strings.TrimSuffix(cachePath, "/")
	}
	cachePath += "/cache"
	tokenCache = fastcache.LoadFromFileOrNew(cachePath+"/token", cacheMaxBytes/4)
	availabilityCache = fastcache.LoadFromFileOrNew(cachePath+"/availability", cacheMaxBytes/4)
	torrentCache = fastcache.LoadFromFileOrNew(cachePath+"/torrent", cacheMaxBytes/4)
	redirectCache = fastcache.LoadFromFileOrNew(cachePath+"/redirect", cacheMaxBytes/4)

	// Basic middleware and health endpoint

	log.Println("Setting up server")
	r := mux.NewRouter()
	s := r.Methods("GET").Subrouter()
	s.Use(timerMiddleware,
		corsMiddleware, // Stremio doesn't show stream responses when no CORS middleware is used!
		handlers.ProxyHeaders,
		recoveryMiddleware,
		loggingMiddleware)
	s.HandleFunc("/health", healthHandler)

	// Stremio endpoints

	conversionClient := realdebrid.NewClient(5*time.Second, tokenCache, availabilityCache)
	searchClient := imdb2torrent.NewClient(5*time.Second, torrentCache)
	// Use token middleware only for the Stremio endpoints
	tokenMiddleware := createTokenMiddleware(conversionClient)
	manifestHandler := createManifestHandler(conversionClient)
	streamHandler := createStreamHandler(searchClient, conversionClient, redirectCache)
	s.HandleFunc("/{apitoken}/manifest.json", tokenMiddleware(manifestHandler).ServeHTTP)
	s.HandleFunc("/{apitoken}/stream/{type}/{id}.json", tokenMiddleware(streamHandler).ServeHTTP)

	// Additional endpoints

	// Redirects stream URLs (previously sent to Stremio) to the actual RealDebrid stream URLs
	s.HandleFunc("/redirect/{id}", createRedirectHandler(redirectCache, conversionClient))

	srv := &http.Server{
		Addr:    bindAddr + ":" + strconv.Itoa(port),
		Handler: s,
		// Timeouts to avoid Slowloris attacks
		ReadTimeout:    time.Second * 5,
		WriteTimeout:   time.Second * 15,
		IdleTimeout:    time.Second * 60,
		MaxHeaderBytes: 1 << 10, // 1 KB
	}

	stopping := false
	stoppingPtr := &stopping

	log.Println("Starting server")
	go func() {
		if err := srv.ListenAndServe(); err != nil {
			log.Fatal("Couldn't start server:", err)
		}
	}()

	// Timed logger for easier debugging with logs
	go func() {
		for {
			log.Println("...")
			time.Sleep(time.Second)
		}
	}()

	// Save cache to file every hour
	go func() {
		for {
			time.Sleep(time.Hour)
			persistCache(cachePath, stoppingPtr)
		}
	}()

	// Print cache stats every hour
	go func() {
		// Don't run at the same time as the persistence
		time.Sleep(time.Minute)
		stats := fastcache.Stats{}
		for {
			tokenCache.UpdateStats(&stats)
			log.Printf("Token cache stats: %#v\n", stats)
			stats.Reset()
			availabilityCache.UpdateStats(&stats)
			log.Printf("Availability cache stats: %#v\n", stats)
			stats.Reset()
			torrentCache.UpdateStats(&stats)
			log.Printf("Torrent cache stats: %#v\n", stats)
			stats.Reset()
			redirectCache.UpdateStats(&stats)
			log.Printf("Redirect cache stats: %#v\n", stats)
			stats.Reset()

			time.Sleep(time.Hour)
		}
	}()

	// Graceful shutdown

	c := make(chan os.Signal, 1)
	// Accept SIGINT (Ctrl+C) and SIGTERM (`docker stop`)
	signal.Notify(c, os.Interrupt, syscall.SIGTERM)
	sig := <-c
	log.Printf("Received \"%v\" signal. Shutting down...\n", sig)
	*stoppingPtr = true
	// Create a deadline to wait for.
	// Using the same value as the server's `WriteTimeout` would be great, because this would mean that every client could finish his request as he normally could.
	// But `docker stop` only gives us 10 seconds.
	// No need to get the cancel func and defer calling it, because srv.Shutdown() will consider the timeout from the context.
	ctx, _ := context.WithTimeout(context.Background(), 9*time.Second)
	// Doesn't block if no connections, but will otherwise wait until the timeout deadline
	if err := srv.Shutdown(ctx); err != nil {
		log.Fatal("Error shutting down server:", err)
	}
}

func persistCache(cacheFilePath string, stoppingPtr *bool) {
	if *stoppingPtr {
		log.Println("Regular cache persistence triggered, but server is shutting down")
		return
	}

	log.Printf("Persisting caches to \"%v\"...\n", cacheFilePath)
	if err := tokenCache.SaveToFileConcurrent(cacheFilePath+"/token", runtime.NumCPU()); err != nil {
		log.Println("Couldn't save token cache to file:", err)
	}
	if err := availabilityCache.SaveToFileConcurrent(cacheFilePath+"/availability", runtime.NumCPU()); err != nil {
		log.Println("Couldn't save availability cache to file:", err)
	}
	if err := torrentCache.SaveToFileConcurrent(cacheFilePath+"/torrent", runtime.NumCPU()); err != nil {
		log.Println("Couldn't save torrent cache to file:", err)
	}
	if err := redirectCache.SaveToFileConcurrent(cacheFilePath+"/redirect", runtime.NumCPU()); err != nil {
		log.Println("Couldn't save redirect cache to file:", err)
	}
	log.Println("Persisted caches")
}
