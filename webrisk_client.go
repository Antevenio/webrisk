// Copyright 2023 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//	https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
// Package webrisk implements a client for the Web Risk API v4.
//
// At a high-level, the implementation does the following:
//
//	            hash(query)
//	                 |
//	            _____V_____
//	           |           | No
//	           | Database  |-----+
//	           |___________|     |
//	                 |           |
//	                 | Maybe?    |
//	            _____V_____      |
//	       Yes |           | No  V
//	     +-----|   Cache   |---->+
//	     |     |___________|     |
//	     |           |           |
//	     |           | Maybe?    |
//	     |      _____V_____      |
//	     V Yes |           | No  V
//	     +<----|    API    |---->+
//	     |     |___________|     |
//	     V                       V
//	(Yes, unsafe)            (No, safe)
//
// Essentially the query is presented to three major components: The database,
// the cache, and the API. Each of these may satisfy the query immediately,
// or may say that it does not know and that the query should be satisfied by
// the next component. The goal of the database and cache is to satisfy as many
// queries as possible to avoid using the API.
//
// Starting with a user query, a hash of the query is performed to preserve
// privacy regarded the exact nature of the query. For example, if the query
// was for a URL, then this would be the SHA256 hash of the URL in question.
//
// Given a query hash, we first check the local database (which is periodically
// synced with the global Web Risk API servers). This database will either
// tell us that the query is definitely safe, or that it does not have
// enough information.
//
// If we are unsure about the query, we check the local cache, which can be used
// to satisfy queries immediately if the same query had been made recently.
// The cache will tell us that the query is either safe, unsafe, or unknown
// (because the it's not in the cache or the entry expired).
//
// If we are still unsure about the query, then we finally query the API server,
// which is guaranteed to return to us an authoritative answer, assuming no
// networking failures.
package webrisk

import (
	"context"
	"errors"
	"io"
	"io/ioutil"
	"log"
	"strings"
	"sync/atomic"
	"time"

	pb "github.com/google/webrisk/internal/webrisk_proto"
)

const (
	// DefaultServerURL is the default URL for the Web Risk API.
	DefaultServerURL = "webrisk.googleapis.com"

	// DefaultUpdatePeriod is the default period for how often UpdateClient will
	// reload its blocklist database.
	DefaultUpdatePeriod = 30 * time.Minute

	// DefaultID is the client ID sent with each API call.
	DefaultID = "WebRiskContainer"
	// DefaultVersion is the Version sent with each API call.
	DefaultVersion = "1.0.0"

	// DefaultRequestTimeout is the default amount of time a single
	// api request can take.
	DefaultRequestTimeout = time.Minute
)

// Errors specific to this package.
var (
	errClosed = errors.New("webrisk: handler is closed")
	errStale  = errors.New("webrisk: threat list is stale")
)

// ThreatType is an enumeration type for threats classes. Examples of threat
// classes are malware, social engineering, etc.
type ThreatType uint16

func (tt ThreatType) String() string { return pb.ThreatType(tt).String() }

// List of ThreatType constants.
const (
	ThreatTypeUnspecified               = ThreatType(pb.ThreatType_THREAT_TYPE_UNSPECIFIED)
	ThreatTypeMalware                   = ThreatType(pb.ThreatType_MALWARE)
	ThreatTypeSocialEngineering         = ThreatType(pb.ThreatType_SOCIAL_ENGINEERING)
	ThreatTypeUnwantedSoftware          = ThreatType(pb.ThreatType_UNWANTED_SOFTWARE)
	ThreatTypeSocialEngineeringExtended = ThreatType(pb.ThreatType_SOCIAL_ENGINEERING_EXTENDED_COVERAGE)
)

// DefaultThreatLists is the default list of threat lists that UpdateClient
// will maintain. If you modify this variable, you must refresh all saved database files.
var DefaultThreatLists = []ThreatType{
	ThreatTypeMalware,
	ThreatTypeSocialEngineering,
	ThreatTypeUnwantedSoftware,
	ThreatTypeSocialEngineeringExtended,
}

// A URLThreat is a specialized ThreatType for the URL threat
// entry type.
type URLThreat struct {
	Pattern string
	ThreatType
}

// Config sets up the UpdateClient object.
type Config struct {
	// ServerURL is the URL for the Web Risk API server.
	// If empty, it defaults to DefaultServerURL.
	ServerURL string

	// ProxyURL is the URL of the proxy to use for all requests.
	// If empty, the underlying library uses $HTTP_PROXY environment variable.
	ProxyURL string

	// APIKey is the key used to authenticate with the Web Risk API
	// service. This field is required.
	APIKey string

	// ID and Version are client metadata associated with each API request to
	// identify the specific implementation of the client.
	// They are similar in usage to the "User-Agent" in an HTTP request.
	// If empty, these default to DefaultID and DefaultVersion, respectively.
	ID      string
	Version string

	// DBPath is a path to a persistent database file.
	// If empty, UpdateClient operates in a non-persistent manner.
	// This means that blocklist results will not be cached beyond the lifetime
	// of the UpdateClient object.
	DBPath string

	// UpdatePeriod determines how often we update the internal list database.
	// If zero value, it defaults to DefaultUpdatePeriod.
	UpdatePeriod time.Duration

	// ThreatListArg is an optional string that will be parsed into ThreatLists.
	// It is expected that names will be an exact match and comma-separated.
	// For Example: 'MALWARE,SOCIAL_ENGINEERING'.
	// Will also accept 'ALL' and load all threat types.
	// If empty, ThreatLists will be loaded instead.
	ThreatListArg string

	// ThreatLists determines which threat lists that UpdateClient should
	// subscribe to. The threats reported by LookupURLs will only be ones that
	// are specified by this list.
	// If empty, it defaults to DefaultThreatLists.
	ThreatLists []ThreatType

	// RequestTimeout determines the timeout value for the http client.
	RequestTimeout time.Duration

	// Logger is an io.Writer that allows UpdateClient to write debug information
	// intended for human consumption.
	// If empty, no logs will be written.
	Logger io.Writer

	FixedCacheTTL time.Duration

	// compressionTypes indicates how the threat entry sets can be compressed.
	compressionTypes []pb.CompressionType

	api api
	now func() time.Time
}

// setDefaults configures Config to have default parameters.
// It reports whether the current configuration is valid.
func (c *Config) setDefaults() bool {
	if c.ServerURL == "" {
		c.ServerURL = DefaultServerURL
	}
	if len(c.ThreatLists) == 0 {
		c.ThreatLists = DefaultThreatLists
	}
	if c.UpdatePeriod <= 0 {
		c.UpdatePeriod = DefaultUpdatePeriod
	}
	if c.RequestTimeout <= 0 {
		c.RequestTimeout = DefaultRequestTimeout
	}
	if c.compressionTypes == nil {
		c.compressionTypes = []pb.CompressionType{pb.CompressionType_RAW, pb.CompressionType_RICE}
	}

	return true
}

// parseThreatTypes accepts a string of named ThreatTypes and parses it into
// an array of valid types. It is used to load command line arguments.
func parseThreatTypes(args string) ([]ThreatType, error) {
	if args == "" || args == "ALL" {
		return DefaultThreatLists, nil
	}
	r := []ThreatType{}
	for _, v := range strings.Split(args, ",") {
		if v == "ALL" {
			return DefaultThreatLists, nil
		}
		tt := ThreatType(pb.ThreatType_value[v])
		if tt == ThreatTypeUnspecified {
			return nil, errors.New("webrisk: unknown threat type: " + v)
		}
		r = append(r, tt)
	}
	return r, nil
}

func (c Config) copy() Config {
	c2 := c
	c2.ThreatLists = append([]ThreatType(nil), c.ThreatLists...)
	c2.compressionTypes = append([]pb.CompressionType(nil), c.compressionTypes...)
	return c2
}

// UpdateClient is a client implementation of API v4.
//
// It provides a set of lookup methods that allows the user to query whether
// certain entries are considered a threat. The implementation manages all of
// local database and caching that would normally be needed to interact
// with the API server.
type UpdateClient struct {
	stats  Stats // Must be first for 64-bit alignment on non 64-bit systems.
	config Config
	api    api
	db     database
	c      cache

	lists map[ThreatType]bool

	log *log.Logger

	closed uint32
	done   chan bool // Signals that the updater routine should stop
}

// Stats records statistics regarding UpdateClient's operation.
type Stats struct {
	QueriesByDatabase int64         // Number of queries satisfied by the database alone
	QueriesByCache    int64         // Number of queries satisfied by the cache alone
	QueriesByAPI      int64         // Number of queries satisfied by an API call
	QueriesFail       int64         // Number of queries that could not be satisfied
	DatabaseUpdateLag time.Duration // Duration since last *missed* update. 0 if next update is in the future.
}

// NewUpdateClient creates a new UpdateClient.
//
// The conf struct allows the user to configure many aspects of the
// UpdateClient's operation.
func NewUpdateClient(conf Config) (*UpdateClient, error) {
	conf = conf.copy()
	if !conf.setDefaults() {
		return nil, errors.New("webrisk: invalid configuration")
	}

	// Parse threat types if args are passed.
	if conf.ThreatListArg != "" {
		var err error
		var tl []ThreatType
		tl, err = parseThreatTypes(conf.ThreatListArg)
		if err != nil || len(tl) == 0 {
			return nil, err
		}
		conf.ThreatLists = tl
	}

	// Create the SafeBrowsing object.
	if conf.api == nil {
		var err error
		conf.api, err = newNetAPI(conf.ServerURL, conf.APIKey, conf.ProxyURL)
		if err != nil {
			return nil, err
		}
	}
	if conf.now == nil {
		conf.now = time.Now
	}
	wr := &UpdateClient{
		config: conf,
		api:    conf.api,
		c:      cache{now: conf.now},
	}

	// TODO: Verify that config.ThreatLists is a subset of the list obtained
	// by "/v4/threatLists" API endpoint.

	// Convert threat lists slice to a map for O(1) lookup.
	wr.lists = make(map[ThreatType]bool)
	for _, td := range conf.ThreatLists {
		wr.lists[td] = true
	}

	// Setup the logger.
	w := conf.Logger
	if conf.Logger == nil {
		w = ioutil.Discard
	}
	wr.log = log.New(w, "webrisk: ", log.Ldate|log.Ltime|log.Lshortfile)

	delay := time.Duration(0)
	// If database file is provided, use that to initialize.
	if !wr.db.Init(&wr.config, wr.log) {
		ctx, cancel := context.WithTimeout(context.Background(), wr.config.RequestTimeout)
		delay, _ = wr.db.Update(ctx, wr.api)
		cancel()
	} else {
		if age := wr.db.SinceLastUpdate(); age < wr.config.UpdatePeriod {
			delay = wr.config.UpdatePeriod - age
		}
	}

	// Start the background list updater.
	wr.done = make(chan bool)
	go wr.updater(delay)
	return wr, nil
}

// Status reports the status of UpdateClient. It returns some statistics
// regarding the operation, and an error representing the status of its
// internal state. Most errors are transient and will recover themselves
// after some period.
func (wr *UpdateClient) Status() (Stats, error) {
	stats := Stats{
		QueriesByDatabase: atomic.LoadInt64(&wr.stats.QueriesByDatabase),
		QueriesByCache:    atomic.LoadInt64(&wr.stats.QueriesByCache),
		QueriesByAPI:      atomic.LoadInt64(&wr.stats.QueriesByAPI),
		QueriesFail:       atomic.LoadInt64(&wr.stats.QueriesFail),
		DatabaseUpdateLag: wr.db.UpdateLag(),
	}
	return stats, wr.db.Status()
}

// WaitUntilReady blocks until the database is not in an error state.
// Returns nil when the database is ready. Returns an error if the provided
// context is canceled or if the UpdateClient instance is Closed.
func (wr *UpdateClient) WaitUntilReady(ctx context.Context) error {
	if atomic.LoadUint32(&wr.closed) == 1 {
		return errClosed
	}
	select {
	case <-wr.db.Ready():
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-wr.done:
		return errClosed
	}
}

// LookupURLs looks up the provided URLs. It returns a list of threats, one for
// every URL requested, and an error if any occurred. It is safe to call this
// method concurrently.
//
// The outer dimension is across all URLs requested, and will always have the
// same length as urls regardless of whether an error occurs or not.
// The inner dimension is across every fragment that a given URL produces.
// For some URL at index i, one can check for a hit on any blocklist by
// checking if len(threats[i]) > 0.
// The ThreatEntryType field in the inner ThreatType will be set to
// ThreatEntryType_URL as this is a URL lookup.
//
// If an error occurs, the caller should treat the threats list returned as a
// best-effort response to the query. The results may be stale or be partial.
func (wr *UpdateClient) LookupURLs(urls []string) (threats [][]URLThreat, err error) {
	threats, err = wr.LookupURLsContext(context.Background(), urls)
	return threats, err
}

// LookupURLsContext looks up the provided URLs. The request will be canceled
// if the provided Context is canceled, or if Config.RequestTimeout has
// elapsed. It is safe to call this method concurrently.
//
// See LookupURLs for details on the returned results.
func (wr *UpdateClient) LookupURLsContext(ctx context.Context, urls []string) (threats [][]URLThreat, err error) {
	ctx, cancel := context.WithTimeout(ctx, wr.config.RequestTimeout)
	defer cancel()

	threats = make([][]URLThreat, len(urls))

	if atomic.LoadUint32(&wr.closed) != 0 {
		return threats, errClosed
	}
	if err := wr.db.Status(); err != nil {
		wr.log.Printf("inconsistent database: %v", err)
		atomic.AddInt64(&wr.stats.QueriesFail, int64(len(urls)))
		return threats, err
	}

	hashes := make(map[hashPrefix]string)
	hash2idxs := make(map[hashPrefix][]int)

	// Construct the follow-up request being made to the server.
	// In the request, we only ask for partial hashes for privacy reasons.
	var reqs []*pb.SearchHashesRequest
	ttm := make(map[pb.ThreatType]bool)

	for i, url := range urls {
		urlhashes, err := generateHashes(url)
		if err != nil {
			wr.log.Printf("error generating urlhashes: %v", err)
			atomic.AddInt64(&wr.stats.QueriesFail, int64(len(urls)-i))
			return threats, err
		}

		for fullHash, pattern := range urlhashes {
			hash2idxs[fullHash] = append(hash2idxs[fullHash], i)
			_, alreadyRequested := hashes[fullHash]
			hashes[fullHash] = pattern

			// Lookup in database according to threat list.
			partialHash, unsureThreats := wr.db.Lookup(fullHash)
			if len(unsureThreats) == 0 {
				atomic.AddInt64(&wr.stats.QueriesByDatabase, 1)
				continue // There are definitely no threats for this full hash
			}

			// Lookup in cache according to recently seen values.
			cachedThreats, cr := wr.c.Lookup(fullHash)
			switch cr {
			case positiveCacheHit:
				// The cache remembers this full hash as a threat.
				// The threats we return to the client is the set intersection
				// of unsureThreats and cachedThreats.
				for _, td := range unsureThreats {
					if _, ok := cachedThreats[td]; ok {
						threats[i] = append(threats[i], URLThreat{
							Pattern:    pattern,
							ThreatType: td,
						})
					}
				}
				atomic.AddInt64(&wr.stats.QueriesByCache, 1)
			case negativeCacheHit:
				// This is cached as a non-threat.
				atomic.AddInt64(&wr.stats.QueriesByCache, 1)
				continue
			default:
				// The cache knows nothing about this full hash, so we must make
				// a request for it.
				if alreadyRequested {
					continue
				}
				for _, td := range unsureThreats {
					ttm[pb.ThreatType(td)] = true
				}

				tts := []pb.ThreatType{}
				for _, tt := range unsureThreats {
					tts = append(tts, pb.ThreatType(tt))
				}

				reqs = append(reqs, &pb.SearchHashesRequest{
					Url:         url,
					HashPrefix:  []byte(partialHash),
					ThreatTypes: tts,
				})
			}
		}
	}

	for _, req := range reqs {
		// Actually query the Web Risk API for exact full hash matches.
		wr.log.Print("Calling WR API looking for: ", req.Url)
		resp, err := wr.api.UriLookup(ctx, req.Url, req.ThreatTypes)
		if err != nil {
			wr.log.Printf("UriLookup failure: %v", err)
			atomic.AddInt64(&wr.stats.QueriesFail, 1)
			return threats, err
		}

		// Todo: build a SearchHashesResponse out of the SearhUrisResponse and SearchHashesRequest
		shResp := new(pb.SearchHashesResponse)
		shResp.NegativeExpireTime = resp.Threat.ExpireTime

		urlhashes, _ := generateHashes(req.Url)

		for fullHash := range urlhashes {
			shThreat := pb.SearchHashesResponse_ThreatHash{
				ThreatTypes: resp.Threat.ThreatTypes,
				Hash:        []byte(fullHash),
				ExpireTime:  resp.Threat.ExpireTime,
			}
			shResp.Threats = append(shResp.Threats, &shThreat)
		}

		// Update the cache.
		wr.c.Update(req, shResp, wr)

		// Pull the information the client cares about out of the response.
		for _, threat := range shResp.GetThreats() {
			wr.log.Printf("Found one threat: %+v", threat)
			fullHash := hashPrefix(threat.Hash)
			if !fullHash.IsFull() {
				continue
			}
			pattern, ok := hashes[fullHash]
			idxs, findidx := hash2idxs[fullHash]
			if findidx && ok {
				for _, td := range threat.ThreatTypes {
					if !wr.lists[ThreatType(td)] {
						continue
					}
					for _, idx := range idxs {
						threats[idx] = append(threats[idx], URLThreat{
							Pattern:    pattern,
							ThreatType: ThreatType(td),
						})
					}
				}
			}
		}
		atomic.AddInt64(&wr.stats.QueriesByAPI, 1)
	}
	return threats, nil
}

// TODO: Add other types of lookup when available.
//	func (wr *UpdateClient) LookupBinaries(digests []string) (threats []BinaryThreat, err error)
//	func (wr *UpdateClient) LookupAddresses(addrs []string) (threats [][]AddressThreat, err error)

// updater is a blocking method that periodically updates the local database.
// This should be run as a separate goroutine and will be automatically stopped
// when wr.Close is called.
func (wr *UpdateClient) updater(delay time.Duration) {
	for {
		wr.log.Printf("Next update in %v", delay)
		select {
		case <-time.After(delay):
			var ok bool
			ctx, cancel := context.WithTimeout(context.Background(), wr.config.RequestTimeout)
			if delay, ok = wr.db.Update(ctx, wr.api); ok {
				wr.log.Printf("background threat list updated")
				wr.c.Purge()
				wr.log.Printf("cache flushed")
			}
			cancel()

		case <-wr.done:
			return
		}
	}
}

// Close cleans up all resources.
// This method must not be called concurrently with other lookup methods.
func (wr *UpdateClient) Close() error {
	if atomic.LoadUint32(&wr.closed) == 0 {
		atomic.StoreUint32(&wr.closed, 1)
		close(wr.done)
	}
	return nil
}
