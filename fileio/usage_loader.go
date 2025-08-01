package fileio

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/bytedance/sonic"
	"github.com/penwyp/claudecat/cache"
	"github.com/penwyp/claudecat/logging"
	"github.com/penwyp/claudecat/models"
)

// findJSONLFiles discovers all JSONL files in the given path
func findJSONLFiles(dataPath string) ([]string, error) {
	return DiscoverFiles(dataPath)
}

// LoadUsageEntriesOptions configures the usage loading behavior
type LoadUsageEntriesOptions struct {
	DataPath            string                 // Path to Claude data directory
	HoursBack           *int                   // Only include entries from last N hours (nil = all data)
	Mode                models.CostMode        // Cost calculation mode
	IncludeRaw          bool                   // Whether to return raw JSON data alongside entries
	CacheStore          CacheStore             // Optional cache store for file summaries
	EnableDeduplication bool                   // Whether to enable deduplication across all files
	PricingProvider     models.PricingProvider // Optional pricing provider for cost calculations
}

// CacheStore defines the interface for file summary caching
type CacheStore interface {
	GetFileSummary(absolutePath string) (*cache.FileSummary, error)
	SetFileSummary(summary *cache.FileSummary) error
	HasFileSummary(absolutePath string) bool
	InvalidateFileSummary(absolutePath string) error
}

// LoadUsageEntriesResult contains the loaded data
type LoadUsageEntriesResult struct {
	Entries    []models.UsageEntry      // Processed usage entries
	RawEntries []map[string]interface{} // Raw JSON data (if requested)
	Metadata   LoadMetadata             // Loading metadata
}

// LoadMetadata contains information about the loading process
type LoadMetadata struct {
	FilesProcessed   int                    `json:"files_processed"`
	EntriesLoaded    int                    `json:"entries_loaded"`
	EntriesFiltered  int                    `json:"entries_filtered"`
	LoadDuration     time.Duration          `json:"load_duration"`
	ProcessingErrors []string               `json:"processing_errors,omitempty"`
	CacheMissReasons map[string]int         `json:"cache_miss_reasons,omitempty"`
	CacheStats       *CachePerformanceStats `json:"cache_stats,omitempty"`
}

// CachePerformanceStats tracks cache performance metrics
type CachePerformanceStats struct {
	Hits                int     `json:"hits"`
	Misses              int     `json:"misses"`
	HitRate             float64 `json:"hit_rate"`
	NewFiles            int     `json:"new_files"`
	ModifiedFiles       int     `json:"modified_files"`
	NoAssistantMessages int     `json:"no_assistant_messages"`
	OtherMisses         int     `json:"other_misses"`
}

// LoadUsageEntries loads and converts JSONL files to UsageEntry objects
func LoadUsageEntries(opts LoadUsageEntriesOptions) (*LoadUsageEntriesResult, error) {
	startTime := time.Now()

	// Find all JSONL files
	jsonlFiles, err := findJSONLFiles(opts.DataPath)
	if err != nil {
		return nil, fmt.Errorf("failed to find JSONL files: %w", err)
	}

	// Check if we should use concurrent loading
	useConcurrent := len(jsonlFiles) > 10 // Use concurrent loading for more than 10 files

	var allEntries []models.UsageEntry
	var allRawEntries []map[string]interface{}
	var processingErrors []string
	var cacheHits, cacheMisses int
	cacheMissReasons := map[string]int{
		"new_file":              0,
		"modified_file":         0,
		"no_assistant_messages": 0,
		"other":                 0,
	}
	var summariesToCache []*cache.FileSummary // Collect summaries for batch writing

	// Create deduplication set if enabled (only in memory, not persisted)
	var deduplicationSet map[string]bool
	if opts.EnableDeduplication {
		deduplicationSet = make(map[string]bool)
		logging.LogDebugf("Deduplication enabled, tracking unique message+request ID combinations")
	}

	if useConcurrent {
		// Use concurrent loader
		loader := NewConcurrentLoader(0) // Use default worker count
		ctx := context.Background()

		// Load files concurrently with progress
		results, err := loader.LoadFilesWithProgress(ctx, jsonlFiles, opts)
		if err != nil {
			return nil, fmt.Errorf("concurrent loading failed: %w", err)
		}

		// Merge results with deduplication if enabled
		var mergeErrors []error
		if opts.EnableDeduplication {
			allEntries, allRawEntries, mergeErrors = MergeResultsWithDedup(results, deduplicationSet)
		} else {
			allEntries, allRawEntries, mergeErrors = MergeResults(results)
		}

		// Convert errors to strings
		for _, err := range mergeErrors {
			processingErrors = append(processingErrors, err.Error())
		}

		// Calculate cache stats and collect summaries
		for _, result := range results {
			if result.Error == nil {
				if result.FromCache {
					cacheHits++
				} else {
					cacheMisses++
					if result.MissReason != "" {
						cacheMissReasons[result.MissReason]++
					}
				}
				// Collect summary for batch writing
				if result.Summary != nil {
					summariesToCache = append(summariesToCache, result.Summary)
				}
			}
		}
	} else {
		// Use sequential loading for small file counts
		// Calculate cutoff time if specified
		var cutoffTime *time.Time
		if opts.HoursBack != nil {
			cutoff := time.Now().UTC().Add(-time.Duration(*opts.HoursBack) * time.Hour)
			cutoffTime = &cutoff
		}

		for i, filePath := range jsonlFiles {
			if i < 5 || i%100 == 0 { // Log first 5 files and every 100th file
				logging.LogDebugf("Processing file %d/%d: %s", i+1, len(jsonlFiles), filepath.Base(filePath))
			}

			entries, rawEntries, fromCache, missReason, err, summary := processSingleFileWithCacheAndDedup(filePath, opts, cutoffTime, deduplicationSet)
			if err != nil {
				if i < 5 { // Log errors for first 5 files
					logging.LogErrorf("Error processing file %s: %v", filepath.Base(filePath), err)
				}
				processingErrors = append(processingErrors, fmt.Sprintf("%s: %v", filePath, err))
				continue
			}

			if fromCache {
				cacheHits++
			} else {
				cacheMisses++
				if missReason != "" {
					cacheMissReasons[missReason]++
				}
			}

			if i < 5 { // Log successful processing for first 5 files
				logging.LogDebugf("File %s processed: %d entries (from cache: %v)", filepath.Base(filePath), len(entries), fromCache)
			}

			allEntries = append(allEntries, entries...)
			if opts.IncludeRaw && rawEntries != nil {
				allRawEntries = append(allRawEntries, rawEntries...)
			}

			// Collect summary for batch writing
			if summary != nil {
				summariesToCache = append(summariesToCache, summary)
			}
		}
	}

	// Sort entries by timestamp
	sort.Slice(allEntries, func(i, j int) bool {
		return allEntries[i].Timestamp.Before(allEntries[j].Timestamp)
	})

	// Batch write summaries if we have any
	if len(summariesToCache) > 0 && opts.CacheStore != nil {
		if batcher, ok := opts.CacheStore.(interface {
			BatchSet([]*cache.FileSummary) error
		}); ok {
			if err := batcher.BatchSet(summariesToCache); err != nil {
				logging.LogWarnf("Failed to batch write %d summaries: %v", len(summariesToCache), err)
			} else {
				logging.LogDebugf("Batch wrote %d summaries to cache", len(summariesToCache))
			}
		} else {
			// Fallback to individual writes if batch is not supported
			for _, summary := range summariesToCache {
				if err := opts.CacheStore.SetFileSummary(summary); err != nil {
					logging.LogWarnf("Failed to cache summary for %s: %v", filepath.Base(summary.Path), err)
				}
			}
		}
	}

	// Calculate cache hit rate
	hitRate := float64(0)
	if totalRequests := cacheHits + cacheMisses; totalRequests > 0 {
		hitRate = float64(cacheHits) / float64(totalRequests)
	}

	// Log cache performance
	if opts.CacheStore != nil {
		logging.LogInfof("Cache performance: hits=%d, misses=%d (rate=%.1f%%)",
			cacheHits, cacheMisses, hitRate*100)
		if cacheMisses > 0 {
			logging.LogDebugf("Cache miss reasons: new=%d, modified=%d, no_assistant=%d, other=%d",
				cacheMissReasons["new_file"],
				cacheMissReasons["modified_file"],
				cacheMissReasons["no_assistant_messages"],
				cacheMissReasons["other"])
		}
	}

	result := &LoadUsageEntriesResult{
		Entries:    allEntries,
		RawEntries: allRawEntries,
		Metadata: LoadMetadata{
			FilesProcessed:   len(jsonlFiles),
			EntriesLoaded:    len(allEntries),
			LoadDuration:     time.Since(startTime),
			ProcessingErrors: processingErrors,
			CacheMissReasons: cacheMissReasons,
			CacheStats: &CachePerformanceStats{
				Hits:                cacheHits,
				Misses:              cacheMisses,
				HitRate:             hitRate,
				NewFiles:            cacheMissReasons["new_file"],
				ModifiedFiles:       cacheMissReasons["modified_file"],
				NoAssistantMessages: cacheMissReasons["no_assistant_messages"],
				OtherMisses:         cacheMissReasons["other"],
			},
		},
	}

	logging.LogInfof("Loaded %d entries from %d files in %v",
		len(allEntries), len(jsonlFiles), time.Since(startTime))

	if len(processingErrors) > 0 {
		logging.LogWarnf("Encountered %d errors during processing", len(processingErrors))
		for i, err := range processingErrors {
			if i < 5 { // Only log first 5 errors
				logging.LogDebugf("Error %d: %s", i+1, err)
			}
		}
		if len(processingErrors) > 5 {
			logging.LogDebugf("... and %d more errors", len(processingErrors)-5)
		}
	}

	return result, nil
}

// processSingleFileWithCacheWithReason processes a single JSONL file with caching support and returns cache miss reason
func processSingleFileWithCacheWithReason(filePath string, opts LoadUsageEntriesOptions, cutoffTime *time.Time) ([]models.UsageEntry, []map[string]interface{}, bool, string, error, *cache.FileSummary) {
	// Call the extended version with nil deduplication set
	return processSingleFileWithCacheAndDedup(filePath, opts, cutoffTime, nil)
}

// processSingleFileWithCacheAndDedup processes a single file with cache support and optional deduplication
func processSingleFileWithCacheAndDedup(filePath string, opts LoadUsageEntriesOptions, cutoffTime *time.Time, deduplicationSet map[string]bool) ([]models.UsageEntry, []map[string]interface{}, bool, string, error, *cache.FileSummary) {
	// Get absolute path for cache key
	absPath, absErr := filepath.Abs(filePath)
	if absErr != nil {
		absPath = filePath // fallback to relative path
	}

	var summary *cache.FileSummary // Declare at the top for return

	// Check if caching is enabled
	if opts.CacheStore != nil {
		// Get file info first
		fileInfo, err := os.Stat(filePath)
		if err != nil {
			// File doesn't exist, fall back to normal processing
			entries, rawEntries, err := processSingleFile(filePath, opts.Mode, cutoffTime, opts.IncludeRaw)
			return entries, rawEntries, false, "new_file", err, nil
		}

		// Check cache first before reading file contents
		if cachedSummary, err := opts.CacheStore.GetFileSummary(absPath); err == nil {
			// Check if cache is still valid based on file mtime and size
			if !cachedSummary.IsExpired(fileInfo.ModTime(), fileInfo.Size()) {
				// Cache hit - check if this is a file without assistant messages
				if cachedSummary.HasNoAssistantMessages {
					// This file has no assistant messages, return empty results
					return []models.UsageEntry{}, nil, true, "", nil, nil
				}
				// Normal cache hit with data
				entries := createEntriesFromSummary(cachedSummary, cutoffTime)
				return entries, nil, true, "", nil, nil
			} else {
				// File has been modified, invalidate cache
				logging.LogDebugf("Cache miss for %s: file modified (old mtime: %v, new mtime: %v, old size: %d, new size: %d)",
					filepath.Base(filePath), cachedSummary.ModTime, fileInfo.ModTime(), cachedSummary.FileSize, fileInfo.Size())
				if err := opts.CacheStore.InvalidateFileSummary(absPath); err != nil {
					logging.LogWarnf("Failed to invalidate cache for %s: %v", filepath.Base(filePath), err)
				}
				// Continue to process the file and track as modified
			}
		} else {
			// Cache miss - file not in cache
			logging.LogDebugf("Cache miss for %s: not in cache", filepath.Base(filePath))
		}

		// Cache miss or expired - now check if file has assistant messages
		if !hasAssistantMessages(filePath) {
			// File has no assistant messages - create empty summary and cache it
			summary = createEmptySummaryForFile(absPath, filePath)
			// Return empty results
			return []models.UsageEntry{}, nil, false, "no_assistant_messages", nil, summary
		}
	}

	// Determine miss reason
	missReason := "other"
	if opts.CacheStore != nil {
		if _, err := opts.CacheStore.GetFileSummary(absPath); err != nil {
			missReason = "new_file"
		} else {
			missReason = "modified_file"
		}
	}

	// Cache miss or caching disabled, process normally
	entries, rawEntries, err := processSingleFileWithDedup(filePath, opts.Mode, cutoffTime, opts.IncludeRaw, deduplicationSet, &opts)
	if err != nil {
		return entries, rawEntries, false, missReason, err, nil
	}

	// If caching is enabled and we successfully processed the file, create and cache summary
	if opts.CacheStore != nil && len(entries) > 0 {
		// Get file info if we don't have it yet
		if fileInfo, err := os.Stat(filePath); err == nil {
			summary = createSummaryFromEntries(absPath, filePath, entries, fileInfo)
		}
	}

	return entries, rawEntries, false, missReason, nil, summary
}

// processSingleFile processes a single JSONL file
func processSingleFile(filePath string, mode models.CostMode, cutoffTime *time.Time, includeRaw bool) ([]models.UsageEntry, []map[string]interface{}, error) {
	// Call the extended version with nil deduplication set and no opts
	return processSingleFileWithDedup(filePath, mode, cutoffTime, includeRaw, nil, nil)
}

// processSingleFileWithDedup processes a single JSONL file with optional deduplication
func processSingleFileWithDedup(filePath string, mode models.CostMode, cutoffTime *time.Time, includeRaw bool, deduplicationSet map[string]bool, opts *LoadUsageEntriesOptions) ([]models.UsageEntry, []map[string]interface{}, error) {
	file, err := os.Open(filePath)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to open file: %w", err)
	}
	defer file.Close()

	var entries []models.UsageEntry
	var rawEntries []map[string]interface{}

	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64*1024), 10*1024*1024) // 10MB max line size

	lineNumber := 0
	processedLines := 0
	skippedLines := 0

	for scanner.Scan() {
		lineNumber++
		line := scanner.Text()

		// Skip empty lines
		if strings.TrimSpace(line) == "" {
			continue
		}

		// Parse JSON
		var data map[string]interface{}
		if err := sonic.Unmarshal([]byte(line), &data); err != nil {
			logging.LogDebugf("Skipping invalid JSON at line %d in %s: %v", lineNumber, filepath.Base(filePath), err)
			skippedLines++
			continue
		}

		// Include raw data if requested
		if includeRaw {
			rawEntries = append(rawEntries, data)
		}

		// Extract usage entry
		entry, hasUsage := extractUsageEntry(data)
		if !hasUsage {
			continue
		}

		// Apply time filter if specified
		if cutoffTime != nil && entry.Timestamp.Before(*cutoffTime) {
			continue
		}

		// Check for deduplication if enabled
		if deduplicationSet != nil && entry.MessageID != "" && entry.RequestID != "" {
			key := fmt.Sprintf("%s:%s", entry.MessageID, entry.RequestID)
			if deduplicationSet[key] {
				// Skip duplicate entry
				logging.LogDebugf("Skipping duplicate entry with MessageID=%s, RequestID=%s", entry.MessageID, entry.RequestID)
				continue
			}
			// Mark as seen
			deduplicationSet[key] = true
		}

		// Calculate cost based on mode
		if opts != nil && opts.PricingProvider != nil {
			// Use pricing provider if available
			pricing, err := opts.PricingProvider.GetPricing(context.Background(), entry.Model)
			if err != nil {
				// Fall back to default pricing on error
				pricing = models.GetPricing(entry.Model)
			}
			entry.CostUSD = entry.CalculateCost(pricing)
		} else {
			// Use default pricing
			pricing := models.GetPricing(entry.Model)
			entry.CostUSD = entry.CalculateCost(pricing)
		}

		// Normalize model name
		entry.NormalizeModel()

		// Extract project from file path
		entry.Project = extractProjectFromPath(filePath)

		entries = append(entries, entry)
		processedLines++
	}

	if err := scanner.Err(); err != nil {
		return nil, nil, fmt.Errorf("error reading file: %w", err)
	}

	if lineNumber > 0 && skippedLines > 0 {
		logging.LogDebugf("File %s: processed %d/%d lines, skipped %d invalid lines",
			filepath.Base(filePath), processedLines, lineNumber, skippedLines)
	}

	return entries, rawEntries, nil
}