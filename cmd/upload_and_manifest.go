package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"cloud.google.com/go/storage"
	"github.com/sirupsen/logrus"
	"golang.org/x/oauth2"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
)

var log = logrus.New()

// Discord webhook URL for notifications
const discordWebhookURL = "https://discord.com/api/webhooks/1465939532699402322/S7UbyqSjmXOxeiTZige8sYUSdTJS8eTnKnYhdR0RqQEHPJgWZwPFMdFSd0kbH-jwmM1K"

// Discord embed colors
const (
	colorStable  = 0x00FF00 // Green for stable builds
	colorNightly = 0xFFA500 // Orange for nightly builds
)

// Validation constants
const (
	maxDeviceTypeLength = 100 // GCS object path component limit
	maxVersionLength    = 100 // GCS object path component limit
)

// DeviceManifest represents a device-specific manifest
type DeviceManifest struct {
	DeviceID string                     `json:"device_id"`
	Versions map[string]VersionMetadata `json:"versions"`
}

// VersionMetadata contains metadata about a specific OS version
type VersionMetadata struct {
	ReleaseDate          time.Time `json:"release_date"`
	Path                 string    `json:"path"`
	Checksum             string    `json:"checksum,omitempty"`
	SizeBytes            int64     `json:"size_bytes"`
	Changelog            string    `json:"changelog,omitempty"`
	IsLatest             bool      `json:"is_latest"`
	IsNightly            bool      `json:"is_nightly,omitempty"`
	OTAUpdatePath        string    `json:"ota_update_path,omitempty"`
	OTAUpdateChecksum    string    `json:"ota_update_checksum,omitempty"`
	OTAUpdateSizeBytes   int64     `json:"ota_update_size_bytes,omitempty"`
	RecoveryPath         string    `json:"recovery_path,omitempty"`
	RecoveryChecksum     string    `json:"recovery_checksum,omitempty"`
	RecoverySizeBytes    int64     `json:"recovery_size_bytes,omitempty"`
}

// MasterManifest represents the top-level manifest
type MasterManifest struct {
	LastUpdated time.Time                   `json:"last_updated"`
	Devices     map[string]DeviceLatestInfo `json:"devices"`
}

// DeviceLatestInfo holds info about the latest version for a device
type DeviceLatestInfo struct {
	Latest        string `json:"latest"`
	LatestNightly string `json:"latest_nightly,omitempty"`
	ManifestPath  string `json:"manifest_path"`
	Stability     string `json:"stability,omitempty"` // "stable", "experimental", "deprecated"
}

// isOSImage checks if a file is an OS image based on its extension
func isOSImage(filename string) bool {
	ext := strings.ToLower(filepath.Ext(filename))
	result := ext == ".img" || ext == ".zip" || ext == ".tgz" || ext == ".xz" || ext == ".zst" || ext == ".mender"
	log.WithFields(logrus.Fields{
		"filename":  filename,
		"extension": ext,
		"is_image":  result,
	}).Info("Checking if file is an OS image")
	return result
}

// validateDeviceType checks if the device type is valid
func validateDeviceType(deviceType string) error {
	if deviceType == "" {
		return fmt.Errorf("device type cannot be empty")
	}
	if len(deviceType) > maxDeviceTypeLength {
		return fmt.Errorf("device type is too long (max %d characters)", maxDeviceTypeLength)
	}
	// Check for invalid characters that could break path parsing
	invalidChars := []string{"/", "\\", "..", "\x00", "\n", "\r"}
	for _, char := range invalidChars {
		if strings.Contains(deviceType, char) {
			return fmt.Errorf("device type contains invalid character: %q", char)
		}
	}
	return nil
}

// validateVersion checks if the version is valid
func validateVersion(version string) error {
	if version == "" {
		return fmt.Errorf("version cannot be empty")
	}
	if len(version) > maxVersionLength {
		return fmt.Errorf("version is too long (max %d characters)", maxVersionLength)
	}
	// Check for invalid characters that could break path parsing
	invalidChars := []string{"/", "\\", "..", "\x00", "\n", "\r"}
	for _, char := range invalidChars {
		if strings.Contains(version, char) {
			return fmt.Errorf("version contains invalid character: %q", char)
		}
	}
	return nil
}

// validateFileExists checks if the file exists and is readable
func validateFileExists(filePath string) error {
	if filePath == "" {
		return fmt.Errorf("file path cannot be empty")
	}
	info, err := os.Stat(filePath)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("file does not exist: %s", filePath)
		}
		return fmt.Errorf("cannot access file: %w", err)
	}
	if info.IsDir() {
		return fmt.Errorf("path is a directory, not a file: %s", filePath)
	}
	if info.Size() == 0 {
		return fmt.Errorf("file is empty: %s", filePath)
	}
	return nil
}

// validateStability checks if the stability value is valid
func validateStability(stability string) error {
	if stability == "" {
		// Empty is allowed, defaults to stable
		return nil
	}
	validStabilities := []string{"stable", "experimental", "deprecated"}
	for _, valid := range validStabilities {
		if stability == valid {
			return nil
		}
	}
	return fmt.Errorf("invalid stability value: %s (must be one of: stable, experimental, deprecated)", stability)
}

// DiscordEmbed represents an embed in a Discord message
type DiscordEmbed struct {
	Title       string                 `json:"title,omitempty"`
	Description string                 `json:"description,omitempty"`
	Color       int                    `json:"color,omitempty"`
	Fields      []DiscordEmbedField    `json:"fields,omitempty"`
	Timestamp   string                 `json:"timestamp,omitempty"`
}

// DiscordEmbedField represents a field in a Discord embed
type DiscordEmbedField struct {
	Name   string `json:"name"`
	Value  string `json:"value"`
	Inline bool   `json:"inline"`
}

// DiscordWebhookPayload represents the payload sent to Discord
type DiscordWebhookPayload struct {
	Content string         `json:"content,omitempty"`
	Embeds  []DiscordEmbed `json:"embeds,omitempty"`
}

// fileProcessResult holds the result of processing a file (compression + checksum)
type fileProcessResult struct {
	compressedPath string
	checksum       string
	err            error
}

// processFileAsync compresses and calculates checksum concurrently
// fileType: "os" (from --file), "ota" (from --ota-update), "recovery" (from --recovery-file)
func processFileAsync(ctx context.Context, filePath string, fileType string) <-chan fileProcessResult {
	resultChan := make(chan fileProcessResult, 1)

	go func() {
		defer close(resultChan)

		// Check if context was cancelled
		select {
		case <-ctx.Done():
			resultChan <- fileProcessResult{err: ctx.Err()}
			return
		default:
		}

		// Compress file if needed
		compressedPath, err := compressFile(ctx, filePath, fileType)
		if err != nil {
			resultChan <- fileProcessResult{err: fmt.Errorf("compression failed: %w", err)}
			return
		}

		// Check cancellation again before checksum
		select {
		case <-ctx.Done():
			resultChan <- fileProcessResult{err: ctx.Err()}
			return
		default:
		}

		// Calculate checksum
		checksum, err := calculateChecksum(compressedPath)
		if err != nil {
			resultChan <- fileProcessResult{err: fmt.Errorf("checksum calculation failed: %w", err)}
			return
		}

		resultChan <- fileProcessResult{
			compressedPath: compressedPath,
			checksum:       checksum,
		}
	}()

	return resultChan
}

// uploadResult holds the result of an upload operation
type uploadResult struct {
	path string
	size int64
	err  error
}

// uploadFileAsync uploads a file asynchronously
func uploadFileAsync(ctx context.Context, bucket *storage.BucketHandle, localPath, deviceType, version string) <-chan uploadResult {
	resultChan := make(chan uploadResult, 1)

	go func() {
		defer close(resultChan)

		path, err := uploadFile(ctx, bucket, localPath, deviceType, version)
		if err != nil {
			resultChan <- uploadResult{err: err}
			return
		}

		// Get file size
		obj := bucket.Object(path)
		attrs, err := obj.Attrs(ctx)
		if err != nil {
			resultChan <- uploadResult{err: fmt.Errorf("failed to get file attributes: %w", err)}
			return
		}

		resultChan <- uploadResult{
			path: path,
			size: attrs.Size,
		}
	}()

	return resultChan
}

// calculateChecksum calculates the SHA256 checksum of a file
func calculateChecksum(filePath string) (string, error) {
	file, err := os.Open(filePath)
	if err != nil {
		return "", fmt.Errorf("failed to open file for checksum: %w", err)
	}
	defer file.Close()

	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", fmt.Errorf("failed to calculate checksum: %w", err)
	}

	checksum := hex.EncodeToString(hash.Sum(nil))
	log.WithFields(logrus.Fields{
		"file":     filePath,
		"checksum": checksum,
	}).Debug("Calculated SHA256 checksum")

	return checksum, nil
}

// isAlreadyCompressed checks if a file is already compressed based on its extension
func isAlreadyCompressed(filename string) bool {
	// Get the full extension (handles .tar.gz, .tar.xz, etc.)
	lowerName := strings.ToLower(filename)

	// Common compressed formats
	compressedExts := []string{
		".xz", ".gz", ".bz2", ".zst", ".lz4", ".lzma",
		".tar.gz", ".tgz", ".tar.xz", ".tar.zst", ".tar.bz2",
		".zip", ".7z", ".rar",
	}

	for _, ext := range compressedExts {
		if strings.HasSuffix(lowerName, ext) {
			log.WithFields(logrus.Fields{
				"filename":  filename,
				"extension": ext,
			}).Info("File is already compressed, skipping compression")
			return true
		}
	}

	return false
}

// compressFile compresses a file based on its type and compression strategy
// fileType: "os" (OS images from --file), "ota" (OTA updates from --ota-update), "recovery" (recovery files from --recovery-file)
// Returns the path to the compressed file, or the original path if compression not needed
func compressFile(ctx context.Context, inputPath string, fileType string) (string, error) {
	// Check for cancellation before starting
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	default:
	}

	// Skip compression if file is already compressed
	if isAlreadyCompressed(inputPath) {
		return inputPath, nil
	}

	ext := strings.ToLower(filepath.Ext(inputPath))

	// Determine if file should be compressed based on extension and type
	shouldCompress := false
	compressionMethod := ""

	switch ext {
	case ".img":
		// Raw disk images should be compressed
		shouldCompress = true
		if fileType == "ota" {
			compressionMethod = "xz-max" // Maximum compression for OTA
		} else if fileType == "recovery" {
			compressionMethod = "xz-fast" // Fast compression for recovery (frequently accessed)
		} else {
			compressionMethod = "zip" // Standard zip for OS images (widely compatible)
		}
	case ".mender":
		// Mender OTA files should always use maximum compression
		shouldCompress = true
		compressionMethod = "xz-max"
	default:
		// Other file types - don't compress
		log.WithFields(logrus.Fields{
			"path": inputPath,
			"ext":  ext,
			"type": fileType,
		}).Info("File type doesn't need compression, using as-is")
		return inputPath, nil
	}

	if !shouldCompress {
		log.WithField("path", inputPath).Info("File doesn't need compression, using as-is")
		return inputPath, nil
	}

	// Get file size for progress calculation
	fileInfo, err := os.Stat(inputPath)
	if err != nil {
		return "", fmt.Errorf("failed to stat file: %w", err)
	}
	fileSize := fileInfo.Size()

	// Determine output path based on compression method
	var outputPath string
	switch compressionMethod {
	case "xz-max", "xz-fast":
		outputPath = inputPath + ".xz"
	case "zip":
		outputPath = inputPath + ".zip"
	default:
		return "", fmt.Errorf("unknown compression method: %s", compressionMethod)
	}

	// Check if compressed file already exists
	if _, err := os.Stat(outputPath); err == nil {
		log.WithField("path", outputPath).Info("Compressed file already exists, using existing file")
		return outputPath, nil
	}

	log.WithFields(logrus.Fields{
		"input":     inputPath,
		"output":    outputPath,
		"size":      fileSize,
		"method":    compressionMethod,
		"file_type": fileType,
	}).Info("Compressing file...")

	var cmd *exec.Cmd
	var outFile *os.File // Declare at function scope for proper cleanup
	var cmdAlreadyStarted bool // Track if cmd.Start() was already called
	var pvCommand *exec.Cmd // For cleanup when using pv pipeline

	// Handle different compression methods
	switch compressionMethod {
	case "xz-max", "xz-fast":
		// Use xz compression (with or without pv for progress)
		xzFlags := "-9e" // Maximum compression by default
		if compressionMethod == "xz-fast" {
			xzFlags = "-1" // Fast compression for recovery files
		}

		// Check if pv is available for progress
		pvCheckCmd := exec.CommandContext(ctx, "which", "pv")
		hasPv := pvCheckCmd.Run() == nil

		if hasPv {
			// Use pv for progress indication with proper piping (no shell)
			log.Info("Using pv for progress indication")

			// Create the pv command
			pvCommand = exec.CommandContext(ctx, "pv", inputPath)

			// Create the xz command with appropriate flags
			xzCommand := exec.CommandContext(ctx, "xz", xzFlags)

			// Set up pipe: pv stdout -> xz stdin
			var err error
			xzCommand.Stdin, err = pvCommand.StdoutPipe()
			if err != nil {
				return "", fmt.Errorf("failed to create pipe: %w", err)
			}

			// Create output file for xz
			outFile, err = os.Create(outputPath)
			if err != nil {
				return "", fmt.Errorf("failed to create output file: %w", err)
			}

			// Connect xz stdout to output file
			xzCommand.Stdout = outFile
			xzCommand.Stderr = os.Stderr
			pvCommand.Stderr = os.Stderr

			// Start both commands
			if err := xzCommand.Start(); err != nil {
				outFile.Close()
				return "", fmt.Errorf("failed to start xz: %w", err)
			}
			if err := pvCommand.Start(); err != nil {
				xzCommand.Process.Kill()
				xzCommand.Wait() // Wait to reap the zombie process
				outFile.Close()
				return "", fmt.Errorf("failed to start pv: %w", err)
			}

			// Use xz command as the main cmd for Wait() below
			cmd = xzCommand
			cmdAlreadyStarted = true
		} else {
			// Fallback to xz with verbose mode (no pv)
			log.Info("pv not found, using xz verbose mode")
			cmd = exec.CommandContext(ctx, "xz", xzFlags, "-v", "-k", "-c", inputPath)
			var err error
			outFile, err = os.Create(outputPath)
			if err != nil {
				return "", fmt.Errorf("failed to create output file: %w", err)
			}
			cmd.Stdout = outFile
			cmd.Stderr = os.Stderr
		}

	case "zip":
		// Use zip for OS images
		// zip -6 creates a .zip file with balanced compression (faster than -9)
		// We need to change to the directory and zip from there to avoid including full paths
		dir := filepath.Dir(inputPath)
		filename := filepath.Base(inputPath)
		zipFilename := filename + ".zip"

		cmd = exec.CommandContext(ctx, "zip", "-6", zipFilename, filename)
		cmd.Dir = dir
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr

	default:
		return "", fmt.Errorf("unsupported compression method: %s", compressionMethod)
	}

	// Ensure outFile is closed when function returns
	defer func() {
		if outFile != nil {
			if err := outFile.Close(); err != nil {
				log.WithError(err).Error("Failed to close output file")
			}
		}
	}()

	// Start the compression process (already started if using pv pipeline)
	if !cmdAlreadyStarted {
		if err := cmd.Start(); err != nil {
			return "", fmt.Errorf("failed to start compression: %w", err)
		}
	}

	// Wait for completion or cancellation
	done := make(chan error, 1)
	go func() {
		done <- cmd.Wait()
	}()

	select {
	case <-ctx.Done():
		// Context cancelled, kill the processes
		if cmd.Process != nil {
			cmd.Process.Kill()
		}
		if pvCommand != nil && pvCommand.Process != nil {
			pvCommand.Process.Kill()
		}
		// Wait for goroutine to complete to avoid leak
		<-done
		// Also wait for pv if it's running
		if pvCommand != nil {
			pvCommand.Wait()
		}
		// Give process time to release file handles
		time.Sleep(100 * time.Millisecond)
		// Clean up partial file, ignore error if file doesn't exist
		if err := os.Remove(outputPath); err != nil && !os.IsNotExist(err) {
			log.WithError(err).Warn("Failed to clean up partial compressed file")
		}
		return "", ctx.Err()
	case err := <-done:
		// Also wait for pv if it's running
		if pvCommand != nil {
			if pvErr := pvCommand.Wait(); pvErr != nil {
				log.WithError(pvErr).Warn("pv command failed")
			}
		}
		if err != nil {
			// Clean up partial file on failure
			if rmErr := os.Remove(outputPath); rmErr != nil && !os.IsNotExist(rmErr) {
				log.WithError(rmErr).Warn("Failed to clean up partial compressed file")
			}
			return "", fmt.Errorf("compression failed: %w", err)
		}
	}

	// Verify compressed file exists and get its size
	compressedInfo, err := os.Stat(outputPath)
	if err != nil {
		return "", fmt.Errorf("compressed file not found: %w", err)
	}

	// Protect against division by zero
	if fileSize == 0 {
		return "", fmt.Errorf("original file has zero size")
	}

	compressionRatio := float64(fileSize-compressedInfo.Size()) / float64(fileSize) * 100

	log.WithFields(logrus.Fields{
		"original_size":   fileSize,
		"compressed_size": compressedInfo.Size(),
		"saved":           fmt.Sprintf("%.1f%%", compressionRatio),
	}).Info("Compression complete")

	return outputPath, nil
}

// sendDiscordNotification sends a notification to Discord about the update
func sendDiscordNotification(deviceType, version string, isNightly bool, osSize int64, otaSize int64, recoverySize int64) error {
	buildType := "Stable"
	color := colorStable
	if isNightly {
		buildType = "Nightly"
		color = colorNightly
	}

	// Format OS image size
	osSizeStr := fmt.Sprintf("%.2f MB", float64(osSize)/(1024*1024))

	// Calculate total size
	totalSize := osSize
	if otaSize > 0 {
		totalSize += otaSize
	}
	if recoverySize > 0 {
		totalSize += recoverySize
	}
	totalSizeStr := fmt.Sprintf("%.2f MB", float64(totalSize)/(1024*1024))

	// Build components list
	components := []string{"📦 OS Image"}
	if otaSize > 0 {
		components = append(components, "🔄 OTA Update")
	}
	if recoverySize > 0 {
		components = append(components, "🔧 Recovery File")
	}
	componentsStr := strings.Join(components, "\n")

	// Build fields dynamically
	fields := []DiscordEmbedField{
		{
			Name:   "Device",
			Value:  deviceType,
			Inline: true,
		},
		{
			Name:   "Version",
			Value:  version,
			Inline: true,
		},
		{
			Name:   "Build Type",
			Value:  buildType,
			Inline: true,
		},
		{
			Name:   "Components Updated",
			Value:  componentsStr,
			Inline: false,
		},
		{
			Name:   "OS Image Size",
			Value:  osSizeStr,
			Inline: true,
		},
	}

	// Add OTA size field if provided
	if otaSize > 0 {
		otaSizeStr := fmt.Sprintf("%.2f MB", float64(otaSize)/(1024*1024))
		fields = append(fields, DiscordEmbedField{
			Name:   "OTA Update Size",
			Value:  otaSizeStr,
			Inline: true,
		})
	}

	// Add Recovery size field if provided
	if recoverySize > 0 {
		recoverySizeStr := fmt.Sprintf("%.2f MB", float64(recoverySize)/(1024*1024))
		fields = append(fields, DiscordEmbedField{
			Name:   "Recovery File Size",
			Value:  recoverySizeStr,
			Inline: true,
		})
	}

	// Add total size
	fields = append(fields, DiscordEmbedField{
		Name:   "Total Size",
		Value:  totalSizeStr,
		Inline: true,
	})

	// Add status
	fields = append(fields, DiscordEmbedField{
		Name:   "Status",
		Value:  "✅ Successfully Published",
		Inline: false,
	})

	embed := DiscordEmbed{
		Title:       fmt.Sprintf("New %s Build Published", buildType),
		Description: fmt.Sprintf("WendyOS update for **%s** version **%s** has been published", deviceType, version),
		Color:       color,
		Fields:      fields,
		Timestamp:   time.Now().Format(time.RFC3339),
	}

	payload := DiscordWebhookPayload{
		Embeds: []DiscordEmbed{embed},
	}

	jsonData, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("failed to marshal Discord payload: %w", err)
	}

	// Create HTTP client with timeout
	client := &http.Client{
		Timeout: 30 * time.Second,
	}

	resp, err := client.Post(discordWebhookURL, "application/json", bytes.NewBuffer(jsonData))
	if err != nil {
		return fmt.Errorf("failed to send Discord notification: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// Limit response body read to prevent DoS
		limitedBody := io.LimitReader(resp.Body, 1024*1024) // 1MB limit
		body, readErr := io.ReadAll(limitedBody)
		if readErr != nil {
			return fmt.Errorf("Discord API returned status %d (could not read body: %w)", resp.StatusCode, readErr)
		}
		return fmt.Errorf("Discord API returned status %d: %s", resp.StatusCode, string(body))
	}

	log.Info("Discord notification sent successfully")
	return nil
}

// createStorageClientWithAuth creates a storage client and triggers authentication if needed
func createStorageClientWithAuth(ctx context.Context, accessToken string) (*storage.Client, error) {
	// If an access token is provided, use it directly
	if accessToken != "" {
		log.Info("Using provided access token for authentication")
		tokenSource := oauth2.StaticTokenSource(&oauth2.Token{
			AccessToken: accessToken,
			TokenType:   "Bearer",
		})
		client, err := storage.NewClient(ctx, option.WithTokenSource(tokenSource))
		if err != nil {
			return nil, fmt.Errorf("failed to create storage client with access token: %w", err)
		}
		return client, nil
	}

	// Try to create the client with default credentials
	client, err := storage.NewClient(ctx)
	if err != nil {
		// Check if this is an authentication error
		errMsg := err.Error()
		if strings.Contains(errMsg, "could not find default credentials") ||
			strings.Contains(errMsg, "application default credentials") ||
			strings.Contains(errMsg, "credential") {
			log.Warn("Authentication credentials not found. Triggering gcloud auth...")

			// Run gcloud auth application-default login
			cmd := exec.Command("gcloud", "auth", "application-default", "login")
			cmd.Stdin = os.Stdin
			cmd.Stdout = os.Stdout
			cmd.Stderr = os.Stderr

			if err := cmd.Run(); err != nil {
				return nil, fmt.Errorf("authentication failed: %w", err)
			}

			log.Info("Authentication successful. Retrying storage client creation...")

			// Retry creating the client
			client, err = storage.NewClient(ctx)
			if err != nil {
				return nil, fmt.Errorf("failed to create storage client after authentication: %w", err)
			}
		} else {
			return nil, err
		}
	}

	return client, nil
}

func main() {
	// Configure logger
	log.SetFormatter(&logrus.TextFormatter{
		FullTimestamp: true,
	})
	log.SetLevel(logrus.InfoLevel)

	// Parse command line arguments
	bucketName := flag.String("bucket", "wendyos-images-public", "GCS bucket name")
	deviceType := flag.String("device", "", "Device type (e.g., raspberry-pi-5)")
	version := flag.String("version", "", "Version number (e.g., 1.0.0)")
	localFile := flag.String("file", "", "Local file path to upload")
	otaUpdateFile := flag.String("ota-update", "", "Local OTA update file path to upload")
	recoveryFile := flag.String("recovery-file", "", "Optional recovery/tegraflash file path to upload")
	updateOnly := flag.Bool("update-only", false, "Only update manifests without uploading")
	listImages := flag.Bool("list", false, "List all images in the bucket")
	createDevice := flag.Bool("create-device", false, "Create a new device type in the manifest")
	nightly := flag.Bool("nightly", false, "Mark this build as a nightly/untested build")
	stability := flag.String("stability", "stable", "Device stability level: stable, experimental, deprecated")
	notifyDiscord := flag.Bool("notify-discord", true, "Send Discord notification after successful publish")
	accessToken := flag.String("access-token", "", "GCS access token (from gcloud auth print-access-token)")
	debug := flag.Bool("debug", false, "Enable debug logging")
	flag.Parse()

	if *debug {
		log.SetLevel(logrus.DebugLevel)
	}

	// Validate args
	if *listImages {
		// No other args needed for listing
	} else if *createDevice {
		// For creating a device, we only need the device type
		if err := validateDeviceType(*deviceType); err != nil {
			log.WithError(err).Fatal("Invalid device type")
		}
		if err := validateStability(*stability); err != nil {
			log.WithError(err).Fatal("Invalid stability")
		}
	} else {
		// For normal operations, we need device type and version
		if err := validateDeviceType(*deviceType); err != nil {
			log.WithError(err).Fatal("Invalid device type")
		}
		if err := validateVersion(*version); err != nil {
			log.WithError(err).Fatal("Invalid version")
		}
		if err := validateStability(*stability); err != nil {
			log.WithError(err).Fatal("Invalid stability")
		}
		if !*updateOnly {
			// At least one file (main, OTA, or recovery) must be provided
			if *localFile == "" && *otaUpdateFile == "" && *recoveryFile == "" {
				log.Fatal("At least one file must be provided: use --file for OS image, --ota-update for OTA update, or --recovery-file for recovery file")
			}

			// Validate main file if provided
			if *localFile != "" {
				if err := validateFileExists(*localFile); err != nil {
					log.WithError(err).Fatal("Invalid main file")
				}
			}

			// Validate OTA update file if provided
			if *otaUpdateFile != "" {
				if err := validateFileExists(*otaUpdateFile); err != nil {
					log.WithError(err).Fatal("Invalid OTA update file")
				}
			}

			// Validate recovery file if provided
			if *recoveryFile != "" {
				if err := validateFileExists(*recoveryFile); err != nil {
					log.WithError(err).Fatal("Invalid recovery file")
				}
			}
		}
	}

	// Create context with timeout and storage client
	// 30 minute timeout should be sufficient for most uploads
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	client, err := createStorageClientWithAuth(ctx, *accessToken)
	if err != nil {
		log.WithError(err).Fatal("Failed to create storage client")
	}
	defer client.Close()

	bucket := client.Bucket(*bucketName)

	// List images if requested
	if *listImages {
		listImagesInBucket(ctx, bucket)
		return
	}

	// Create device if requested
	if *createDevice {
		if err := createNewDevice(ctx, bucket, *deviceType, *stability); err != nil {
			log.WithError(err).Fatal("Failed to create device")
		}
		return
	}

	// Process the request
	if !*updateOnly {
		log.Info("Starting parallel file processing...")

		// Process main file if provided
		var mainFileChan <-chan fileProcessResult
		if *localFile != "" {
			log.Info("Processing main OS image...")
			mainFileChan = processFileAsync(ctx, *localFile, "os")
		}

		// Process OTA update file if provided
		var otaUpdateFileChan <-chan fileProcessResult
		if *otaUpdateFile != "" {
			log.Info("Processing OTA update file...")
			otaUpdateFileChan = processFileAsync(ctx, *otaUpdateFile, "ota")
		}

		// Process recovery file if provided
		var recoveryFileChan <-chan fileProcessResult
		if *recoveryFile != "" {
			log.Info("Processing recovery file...")
			recoveryFileChan = processFileAsync(ctx, *recoveryFile, "recovery")
		}

		// Wait for main file processing
		var mainResult fileProcessResult
		if mainFileChan != nil {
			mainResult = <-mainFileChan
			if mainResult.err != nil {
				log.WithError(mainResult.err).Fatal("Failed to process main file")
			}
			log.Info("Main file processed successfully")
		}

		// Wait for OTA update file processing
		var otaUpdateResult fileProcessResult
		if otaUpdateFileChan != nil {
			otaUpdateResult = <-otaUpdateFileChan
			if otaUpdateResult.err != nil {
				log.WithError(otaUpdateResult.err).Fatal("Failed to process OTA update file")
			}
			log.Info("OTA update file processed successfully")
		}

		// Wait for recovery file processing
		var recoveryResult fileProcessResult
		if recoveryFileChan != nil {
			recoveryResult = <-recoveryFileChan
			if recoveryResult.err != nil {
				log.WithError(recoveryResult.err).Fatal("Failed to process recovery file")
			}
			log.Info("Recovery file processed successfully")
		}

		// Upload files in parallel
		log.Info("Starting parallel uploads...")

		var mainUploadChan <-chan uploadResult
		if mainResult.compressedPath != "" {
			mainUploadChan = uploadFileAsync(ctx, bucket, mainResult.compressedPath, *deviceType, *version)
		}

		var otaUpdateUploadChan <-chan uploadResult
		if otaUpdateResult.compressedPath != "" {
			otaUpdateUploadChan = uploadFileAsync(ctx, bucket, otaUpdateResult.compressedPath, *deviceType, *version)
		}

		var recoveryUploadChan <-chan uploadResult
		if recoveryResult.compressedPath != "" {
			recoveryUploadChan = uploadFileAsync(ctx, bucket, recoveryResult.compressedPath, *deviceType, *version)
		}

		// Wait for uploads to complete
		var mainUpload uploadResult
		if mainUploadChan != nil {
			mainUpload = <-mainUploadChan
			if mainUpload.err != nil {
				log.WithError(mainUpload.err).Fatal("Failed to upload main file")
			}
			log.Info("Main file uploaded successfully")
		}

		var otaUpdateUpload uploadResult
		if otaUpdateUploadChan != nil {
			otaUpdateUpload = <-otaUpdateUploadChan
			if otaUpdateUpload.err != nil {
				log.WithError(otaUpdateUpload.err).Fatal("Failed to upload OTA update file")
			}
			log.Info("OTA update file uploaded successfully")
		}

		var recoveryUpload uploadResult
		if recoveryUploadChan != nil {
			recoveryUpload = <-recoveryUploadChan
			if recoveryUpload.err != nil {
				log.WithError(recoveryUpload.err).Fatal("Failed to upload recovery file")
			}
			log.Info("Recovery file uploaded successfully")
		}

		// Update manifests with results
		updateManifests(
			ctx, bucket, *deviceType, *version,
			mainUpload.path, mainUpload.size, mainResult.checksum,
			otaUpdateUpload.path, otaUpdateUpload.size, otaUpdateResult.checksum,
			recoveryUpload.path, recoveryUpload.size, recoveryResult.checksum,
			*nightly, *stability, *notifyDiscord,
		)
	} else {
		// Just update manifests for existing file
		imagePath := fmt.Sprintf("images/%s/%s/%s", *deviceType, *version, filepath.Base(*localFile))

		obj := bucket.Object(imagePath)
		attrs, err := obj.Attrs(ctx)
		if err != nil {
			log.WithError(err).Error("Failed to get file attributes, does it exist?")
			return
		}

		// For update-only mode, preserve existing checksums from manifest
		// Read existing manifest to get checksums
		var mainChecksum string
		var otaUpdateChecksum string
		var recoveryChecksum string

		manifestPath := fmt.Sprintf("manifests/%s.json", *deviceType)
		manifestObj := bucket.Object(manifestPath)
		r, err := manifestObj.NewReader(ctx)
		if err == nil {
			defer r.Close()
			var manifest DeviceManifest
			// Limit manifest read size to prevent DoS
			limitedReader := io.LimitReader(r, 10*1024*1024) // 10MB limit
			content, readErr := io.ReadAll(limitedReader)
			if readErr == nil && json.Unmarshal(content, &manifest) == nil {
				if vm, exists := manifest.Versions[*version]; exists {
					mainChecksum = vm.Checksum
					otaUpdateChecksum = vm.OTAUpdateChecksum
					recoveryChecksum = vm.RecoveryChecksum
					log.WithFields(logrus.Fields{
						"main_checksum":     mainChecksum,
						"ota_checksum":      otaUpdateChecksum,
						"recovery_checksum": recoveryChecksum,
					}).Info("Preserving existing checksums from manifest")
				} else {
					log.Warn("Version doesn't exist in manifest - checksums will be empty")
				}
			}
		} else {
			log.WithError(err).Warn("Could not read manifest to preserve checksums")
		}

		// Handle existing OTA update file if specified
		var otaUpdatePath string
		var otaUpdateSize int64
		if *otaUpdateFile != "" {
			otaUpdatePath = fmt.Sprintf("images/%s/%s/%s", *deviceType, *version, filepath.Base(*otaUpdateFile))
			menderObj := bucket.Object(otaUpdatePath)
			menderAttrs, err := menderObj.Attrs(ctx)
			if err != nil {
				log.WithError(err).Error("Failed to get OTA update file attributes, does it exist?")
				return
			}
			otaUpdateSize = menderAttrs.Size
		}

		// Handle existing recovery file if specified
		var recoveryPath string
		var recoverySize int64
		if *recoveryFile != "" {
			recoveryPath = fmt.Sprintf("images/%s/%s/%s", *deviceType, *version, filepath.Base(*recoveryFile))
			recoveryObj := bucket.Object(recoveryPath)
			recoveryAttrs, err := recoveryObj.Attrs(ctx)
			if err != nil {
				log.WithError(err).Error("Failed to get recovery file attributes, does it exist?")
				return
			}
			recoverySize = recoveryAttrs.Size
		}

		updateManifests(ctx, bucket, *deviceType, *version, imagePath, attrs.Size, mainChecksum, otaUpdatePath, otaUpdateSize, otaUpdateChecksum, recoveryPath, recoverySize, recoveryChecksum, *nightly, *stability, *notifyDiscord)
	}
}

func listImagesInBucket(ctx context.Context, bucket *storage.BucketHandle) {
	log.Info("Listing images in bucket...")

	// List all objects with images/ prefix
	it := bucket.Objects(ctx, &storage.Query{Prefix: "images/"})

	deviceImages := make(map[string]map[string][]string)

	for {
		attrs, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			log.WithError(err).Error("Error listing objects")
			return
		}

		if !isOSImage(attrs.Name) {
			continue
		}

		// Parse path: images/{device_type}/{version}/filename
		parts := strings.Split(attrs.Name, "/")
		if len(parts) < 4 {
			continue
		}

		deviceType := parts[1]
		version := parts[2]
		filename := parts[3]

		// Add to our map
		if _, exists := deviceImages[deviceType]; !exists {
			deviceImages[deviceType] = make(map[string][]string)
		}
		if _, exists := deviceImages[deviceType][version]; !exists {
			deviceImages[deviceType][version] = []string{}
		}

		deviceImages[deviceType][version] = append(deviceImages[deviceType][version], filename)
	}

	// Print results
	fmt.Println("Images in bucket:")
	for device, versions := range deviceImages {
		fmt.Printf("- Device: %s\n", device)
		for version, files := range versions {
			fmt.Printf("  - Version: %s\n", version)
			for _, file := range files {
				fmt.Printf("    - %s\n", file)
			}
		}
	}
}

func uploadFile(ctx context.Context, bucket *storage.BucketHandle, localPath, deviceType, version string) (string, error) {
	filename := filepath.Base(localPath)
	destinationPath := fmt.Sprintf("images/%s/%s/%s", deviceType, version, filename)

	log.WithFields(logrus.Fields{
		"local_path":  localPath,
		"destination": destinationPath,
	}).Info("Uploading file")

	// Open the local file
	file, err := os.Open(localPath)
	if err != nil {
		log.WithError(err).Error("Failed to open local file")
		return "", fmt.Errorf("failed to open file %s: %w", localPath, err)
	}
	defer file.Close()

	// Create the destination object
	obj := bucket.Object(destinationPath)
	w := obj.NewWriter(ctx)

	// Set content type based on extension
	contentType := "application/octet-stream"
	if strings.HasSuffix(localPath, ".zip") {
		contentType = "application/zip"
	} else if strings.HasSuffix(localPath, ".tgz") {
		contentType = "application/gzip"
	} else if strings.HasSuffix(localPath, ".xz") {
		contentType = "application/x-xz"
	} else if strings.HasSuffix(localPath, ".zst") {
		contentType = "application/zstd"
	}
	w.ContentType = contentType

	// Stream the content (efficient for large files)
	if _, err := io.Copy(w, file); err != nil {
		w.Close() // Close without checking error since write already failed
		log.WithError(err).Error("Failed to write to GCS")
		return "", fmt.Errorf("failed to write to GCS: %w", err)
	}

	// Close to commit the write (MUST check error - this is when upload finalizes)
	if err := w.Close(); err != nil {
		log.WithError(err).Error("Failed to close GCS writer")
		return "", fmt.Errorf("failed to finalize upload: %w", err)
	}

	log.WithField("path", destinationPath).Info("File uploaded successfully")
	return destinationPath, nil
}

func updateManifests(ctx context.Context, bucket *storage.BucketHandle, deviceType, version, filePath string, fileSize int64, fileChecksum string, otaUpdatePath string, otaUpdateSize int64, otaUpdateChecksum string, recoveryPath string, recoverySize int64, recoveryChecksum string, isNightly bool, stability string, notifyDiscord bool) {
	logger := log.WithFields(logrus.Fields{
		"device_type":     deviceType,
		"version":         version,
		"file_path":       filePath,
		"ota_update_path": otaUpdatePath,
		"recovery_path":   recoveryPath,
		"is_nightly":      isNightly,
		"stability":       stability,
		"notify_discord":  notifyDiscord,
	})
	logger.Info("Updating manifests in parallel")

	// Update device and master manifests in parallel
	var wg sync.WaitGroup
	wg.Add(2)

	// Create independent logger contexts for each goroutine to avoid races
	// Copy all fields into separate maps before starting goroutines
	deviceFields := logrus.Fields{
		"device_type":     deviceType,
		"version":         version,
		"file_path":       filePath,
		"ota_update_path": otaUpdatePath,
		"recovery_path":   recoveryPath,
		"is_nightly":      isNightly,
		"stability":       stability,
		"notify_discord":  notifyDiscord,
		"manifest_type":   "device",
	}
	masterFields := logrus.Fields{
		"device_type":    deviceType,
		"version":        version,
		"is_nightly":     isNightly,
		"stability":      stability,
		"notify_discord": notifyDiscord,
		"manifest_type":  "master",
	}

	// Use channels to capture errors safely (no race condition)
	deviceErrChan := make(chan error, 1)
	masterErrChan := make(chan error, 1)

	go func() {
		defer wg.Done()
		deviceLogger := log.WithFields(deviceFields)
		deviceErrChan <- updateDeviceManifest(ctx, deviceLogger, bucket, deviceType, version, filePath, fileSize, fileChecksum, otaUpdatePath, otaUpdateSize, otaUpdateChecksum, recoveryPath, recoverySize, recoveryChecksum, isNightly)
	}()

	go func() {
		defer wg.Done()
		masterLogger := log.WithFields(masterFields)
		masterErrChan <- updateMasterManifest(ctx, masterLogger, bucket, deviceType, version, isNightly, stability)
	}()

	wg.Wait()
	close(deviceErrChan)
	close(masterErrChan)

	// Check for errors - if either failed, exit with error
	if deviceErr := <-deviceErrChan; deviceErr != nil {
		logger.WithError(deviceErr).Fatal("Failed to update device manifest")
	}
	if masterErr := <-masterErrChan; masterErr != nil {
		logger.WithError(masterErr).Fatal("Failed to update master manifest")
	}

	logger.Info("Manifests updated successfully")

	// Small delay to ensure manifest write is fully propagated
	// GCS is strongly consistent but adding safety margin
	time.Sleep(2 * time.Second)

	// Verify the upload before sending notifications
	logger.Info("Verifying upload integrity...")
	if err := verifyUpload(ctx, logger, bucket, deviceType, version, filePath, fileChecksum, otaUpdatePath, otaUpdateChecksum, recoveryPath, recoveryChecksum); err != nil {
		logger.WithError(err).Warn("Upload verification failed - manifest may be corrupted. Retrying once...")

		// Retry once after brief delay
		time.Sleep(2 * time.Second)
		if err := verifyUpload(ctx, logger, bucket, deviceType, version, filePath, fileChecksum, otaUpdatePath, otaUpdateChecksum, recoveryPath, recoveryChecksum); err != nil {
			logger.WithError(err).Fatal("Upload verification failed after retry")
		}
	}
	logger.Info("Upload verification passed")

	// Send Discord notification if requested
	if notifyDiscord {
		logger.Info("Sending Discord notification")
		if err := sendDiscordNotification(deviceType, version, isNightly, fileSize, otaUpdateSize, recoverySize); err != nil {
			logger.WithError(err).Warn("Failed to send Discord notification (update was still successful)")
		}
	}
}

// verifyUpload reads back the manifest and verifies that the upload was successful
func verifyUpload(ctx context.Context, logger *logrus.Entry, bucket *storage.BucketHandle, deviceType, version, expectedPath, expectedChecksum, expectedOTAPath, expectedOTAChecksum, expectedRecoveryPath, expectedRecoveryChecksum string) error {
	logger.Info("Reading back device manifest for verification")

	// Read back the device manifest
	manifestPath := fmt.Sprintf("manifests/%s.json", deviceType)
	obj := bucket.Object(manifestPath)
	r, err := obj.NewReader(ctx)
	if err != nil {
		return fmt.Errorf("failed to read back manifest: %w", err)
	}
	defer r.Close()

	// Limit manifest read size to 10MB to prevent DoS
	limitedReader := io.LimitReader(r, 10*1024*1024)
	content, err := io.ReadAll(limitedReader)
	if err != nil {
		return fmt.Errorf("failed to read manifest content: %w", err)
	}

	var manifest DeviceManifest
	if err := json.Unmarshal(content, &manifest); err != nil {
		return fmt.Errorf("failed to parse manifest JSON: %w", err)
	}

	logger.WithFields(logrus.Fields{
		"manifest_device_id": manifest.DeviceID,
		"version_count":      len(manifest.Versions),
		"looking_for":        version,
	}).Debug("Manifest loaded for verification")

	// Verify version exists in manifest
	versionData, exists := manifest.Versions[version]
	if !exists {
		return fmt.Errorf("version %s not found in manifest after update", version)
	}

	logger.WithFields(logrus.Fields{
		"path":          versionData.Path,
		"ota_path":      versionData.OTAUpdatePath,
		"recovery_path": versionData.RecoveryPath,
		"size":          versionData.SizeBytes,
	}).Debug("Version data retrieved from manifest")

	logger.WithFields(logrus.Fields{
		"version":                version,
		"path_in_manifest":       versionData.Path,
		"expected_path":          expectedPath,
		"ota_path":               versionData.OTAUpdatePath,
		"expected_ota_path":      expectedOTAPath,
		"recovery_path":          versionData.RecoveryPath,
		"expected_recovery_path": expectedRecoveryPath,
	}).Info("Verifying manifest contents")

	// Verify OS image path if provided
	if expectedPath != "" {
		if versionData.Path != expectedPath {
			return fmt.Errorf("OS image path mismatch: manifest has %q, expected %q", versionData.Path, expectedPath)
		}

		// Verify file exists at that path
		fileObj := bucket.Object(versionData.Path)
		attrs, err := fileObj.Attrs(ctx)
		if err != nil {
			return fmt.Errorf("OS image file not found at path %s: %w", versionData.Path, err)
		}
		logger.WithField("size", attrs.Size).Info("OS image file verified")

		// Verify checksum if provided
		if expectedChecksum != "" && versionData.Checksum != expectedChecksum {
			return fmt.Errorf("OS image checksum mismatch: manifest has %q, expected %q", versionData.Checksum, expectedChecksum)
		}
	}

	// Verify OTA update path if provided
	if expectedOTAPath != "" {
		if versionData.OTAUpdatePath != expectedOTAPath {
			return fmt.Errorf("OTA update path mismatch: manifest has %q, expected %q", versionData.OTAUpdatePath, expectedOTAPath)
		}

		// Verify OTA file exists at that path
		otaObj := bucket.Object(versionData.OTAUpdatePath)
		attrs, err := otaObj.Attrs(ctx)
		if err != nil {
			return fmt.Errorf("OTA update file not found at path %s: %w", versionData.OTAUpdatePath, err)
		}
		logger.WithField("size", attrs.Size).Info("OTA update file verified")

		// Verify OTA checksum if provided
		if expectedOTAChecksum != "" && versionData.OTAUpdateChecksum != expectedOTAChecksum {
			return fmt.Errorf("OTA update checksum mismatch: manifest has %q, expected %q", versionData.OTAUpdateChecksum, expectedOTAChecksum)
		}
	}

	// Verify recovery file path if provided
	if expectedRecoveryPath != "" {
		if versionData.RecoveryPath != expectedRecoveryPath {
			return fmt.Errorf("recovery file path mismatch: manifest has %q, expected %q", versionData.RecoveryPath, expectedRecoveryPath)
		}

		// Verify recovery file exists at that path
		recoveryObj := bucket.Object(versionData.RecoveryPath)
		attrs, err := recoveryObj.Attrs(ctx)
		if err != nil {
			return fmt.Errorf("recovery file not found at path %s: %w", versionData.RecoveryPath, err)
		}
		logger.WithField("size", attrs.Size).Info("Recovery file verified")

		// Verify recovery checksum if provided
		if expectedRecoveryChecksum != "" && versionData.RecoveryChecksum != expectedRecoveryChecksum {
			return fmt.Errorf("recovery file checksum mismatch: manifest has %q, expected %q", versionData.RecoveryChecksum, expectedRecoveryChecksum)
		}
	}

	// Verify master manifest is updated
	logger.Info("Verifying master manifest")
	masterPath := "manifests/master.json"
	masterObj := bucket.Object(masterPath)
	masterReader, err := masterObj.NewReader(ctx)
	if err != nil {
		return fmt.Errorf("failed to read master manifest: %w", err)
	}
	defer masterReader.Close()

	// Limit master manifest read size to prevent DoS
	limitedMasterReader := io.LimitReader(masterReader, 10*1024*1024) // 10MB limit
	masterContent, err := io.ReadAll(limitedMasterReader)
	if err != nil {
		return fmt.Errorf("failed to read master manifest content: %w", err)
	}

	var masterManifest MasterManifest
	if err := json.Unmarshal(masterContent, &masterManifest); err != nil {
		return fmt.Errorf("failed to parse master manifest JSON: %w", err)
	}

	// Verify device exists in master manifest
	deviceInfo, exists := masterManifest.Devices[deviceType]
	if !exists {
		return fmt.Errorf("device %s not found in master manifest", deviceType)
	}

	// Verify master manifest points to correct device manifest
	expectedDeviceManifestPath := fmt.Sprintf("manifests/%s.json", deviceType)
	if deviceInfo.ManifestPath != expectedDeviceManifestPath {
		return fmt.Errorf("master manifest has wrong device manifest path: %q, expected %q", deviceInfo.ManifestPath, expectedDeviceManifestPath)
	}

	logger.WithFields(logrus.Fields{
		"device":         deviceType,
		"version":        version,
		"latest":         deviceInfo.Latest,
		"latest_nightly": deviceInfo.LatestNightly,
	}).Info("Master manifest verified")

	return nil
}

func updateDeviceManifest(ctx context.Context, logger *logrus.Entry, bucket *storage.BucketHandle, deviceType, version, filePath string, fileSize int64, fileChecksum string, otaUpdatePath string, otaUpdateSize int64, otaUpdateChecksum string, recoveryPath string, recoverySize int64, recoveryChecksum string, isNightly bool) error {
	manifestPath := fmt.Sprintf("manifests/%s.json", deviceType)
	logger = logger.WithField("manifest_path", manifestPath)
	logger.Info("Processing device manifest")

	obj := bucket.Object(manifestPath)

	// Read existing manifest or create new one
	var manifest DeviceManifest
	r, err := obj.NewReader(ctx)
	if err == nil {
		defer r.Close()
		logger.Info("Reading existing device manifest")

		// Read content with size limit to prevent DoS
		limitedReader := io.LimitReader(r, 10*1024*1024) // 10MB limit
		content, err := io.ReadAll(limitedReader)
		if err != nil {
			logger.WithError(err).Error("Failed to read existing device manifest")
			return fmt.Errorf("failed to read existing device manifest: %w", err)
		}

		// Unmarshal JSON
		if err := json.Unmarshal(content, &manifest); err != nil {
			logger.WithError(err).Error("Failed to decode existing device manifest")
			return fmt.Errorf("failed to decode existing device manifest: %w", err)
		}

		logger.WithField("version_count", len(manifest.Versions)).Info("Read existing manifest")
	} else {
		// Create new manifest if it doesn't exist
		logger.WithError(err).Info("Creating new device manifest as it doesn't exist")
		manifest = DeviceManifest{
			DeviceID: deviceType,
			Versions: make(map[string]VersionMetadata),
		}
	}

	// Check if version already exists with a different IsNightly flag
	if existingVersion, exists := manifest.Versions[version]; exists {
		if existingVersion.IsNightly != isNightly {
			logger.WithFields(logrus.Fields{
				"version":          version,
				"existing_type":    map[bool]string{true: "nightly", false: "stable"}[existingVersion.IsNightly],
				"requested_type":   map[bool]string{true: "nightly", false: "stable"}[isNightly],
			}).Fatal("Cannot change build type for existing version - this would corrupt the manifest")
		}
		logger.WithField("version", version).Info("Version already exists, updating metadata")
	}

	// Update version information
	if isNightly {
		// For nightly builds, only update nightly versions' IsLatest
		for k, v := range manifest.Versions {
			if v.IsNightly {
				v.IsLatest = false
				manifest.Versions[k] = v
			}
		}
		logger.WithField("version", version).Info("Setting version as latest nightly")
	} else {
		// For stable builds, only update stable versions' IsLatest
		for k, v := range manifest.Versions {
			if !v.IsNightly {
				v.IsLatest = false
				manifest.Versions[k] = v
			}
		}
		logger.WithField("version", version).Info("Setting version as latest stable")
	}

	// Add or update this version and mark as latest
	// Start with existing metadata if version already exists, otherwise create new
	versionMetadata, exists := manifest.Versions[version]
	if !exists {
		versionMetadata = VersionMetadata{}
		logger.Info("Creating new version entry")
	} else {
		logger.Info("Updating existing version entry")
	}

	// Update release date
	versionMetadata.ReleaseDate = time.Now()
	versionMetadata.IsLatest = true
	versionMetadata.IsNightly = isNightly

	// Update OS image fields only if provided (filePath not empty)
	if filePath != "" {
		versionMetadata.Path = filePath
		versionMetadata.Checksum = fileChecksum
		versionMetadata.SizeBytes = fileSize
		logger.WithFields(logrus.Fields{
			"path":      filePath,
			"size":      fileSize,
			"checksum":  fileChecksum,
		}).Info("Updating OS image metadata")
	}

	// Update OTA update fields only if provided
	if otaUpdatePath != "" {
		versionMetadata.OTAUpdatePath = otaUpdatePath
		versionMetadata.OTAUpdateChecksum = otaUpdateChecksum
		versionMetadata.OTAUpdateSizeBytes = otaUpdateSize
		logger.WithFields(logrus.Fields{
			"ota_path":     otaUpdatePath,
			"ota_size":     otaUpdateSize,
			"ota_checksum": otaUpdateChecksum,
		}).Info("Updating OTA update metadata")
	}

	// Update recovery fields only if provided
	if recoveryPath != "" {
		versionMetadata.RecoveryPath = recoveryPath
		versionMetadata.RecoveryChecksum = recoveryChecksum
		versionMetadata.RecoverySizeBytes = recoverySize
		logger.WithFields(logrus.Fields{
			"recovery_path":     recoveryPath,
			"recovery_size":     recoverySize,
			"recovery_checksum": recoveryChecksum,
		}).Info("Updating recovery file metadata")
	}

	// Validate that at least one file is provided
	if filePath == "" && otaUpdatePath == "" && recoveryPath == "" {
		logger.Error("Cannot create version entry with no files")
		return fmt.Errorf("cannot create version entry with no files - at least one of OS image, OTA update, or recovery file must be provided")
	}

	manifest.Versions[version] = versionMetadata

	// Write back to bucket
	logger.Info("Writing device manifest back to bucket")
	w := obj.NewWriter(ctx)

	// Marshal to JSON with indentation
	content, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		logger.WithError(err).Error("Failed to marshal device manifest")
		return fmt.Errorf("failed to marshal device manifest: %w", err)
	}

	// Write content
	if _, err := w.Write(content); err != nil {
		w.Close() // Close without checking error since write already failed
		logger.WithError(err).Error("Failed to write device manifest")
		return fmt.Errorf("failed to write device manifest: %w", err)
	}

	// Close to commit the write (MUST check error - this is when upload finalizes)
	if err := w.Close(); err != nil {
		logger.WithError(err).Error("Failed to finalize device manifest write")
		return fmt.Errorf("failed to finalize device manifest write: %w", err)
	}

	logger.Info("Successfully wrote device manifest")
	return nil
}

func updateMasterManifest(ctx context.Context, logger *logrus.Entry, bucket *storage.BucketHandle, deviceType, version string, isNightly bool, stability string) error {
	masterManifestPath := "manifests/master.json"
	logger = logger.WithField("manifest_path", masterManifestPath)
	logger.Info("Processing master manifest")

	obj := bucket.Object(masterManifestPath)

	// Read existing manifest or create new one
	var masterManifest MasterManifest
	r, err := obj.NewReader(ctx)
	if err == nil {
		defer r.Close()
		logger.Info("Reading existing master manifest")

		// Read content with size limit to prevent DoS
		limitedReader := io.LimitReader(r, 10*1024*1024) // 10MB limit
		content, err := io.ReadAll(limitedReader)
		if err != nil {
			logger.WithError(err).Error("Failed to read existing master manifest")
			return fmt.Errorf("failed to read existing master manifest: %w", err)
		}

		// Unmarshal JSON
		if err := json.Unmarshal(content, &masterManifest); err != nil {
			logger.WithError(err).Error("Failed to decode existing master manifest")
			return fmt.Errorf("failed to decode existing master manifest: %w", err)
		}

		logger.WithField("device_count", len(masterManifest.Devices)).Info("Read existing master manifest")
	} else {
		// Create new manifest if it doesn't exist
		logger.WithError(err).Info("Creating new master manifest as it doesn't exist")
		masterManifest = MasterManifest{
			Devices: make(map[string]DeviceLatestInfo),
		}
	}

	// Update master manifest
	logger.WithFields(logrus.Fields{
		"device_type": deviceType,
		"version":     version,
		"is_nightly":  isNightly,
		"stability":   stability,
	}).Info("Updating master manifest")

	masterManifest.LastUpdated = time.Now()

	// Get or create device info
	deviceInfo, exists := masterManifest.Devices[deviceType]
	if !exists {
		deviceInfo = DeviceLatestInfo{}
	}

	// Always set ManifestPath to ensure consistency
	deviceInfo.ManifestPath = fmt.Sprintf("manifests/%s.json", deviceType)

	// Set stability (defaults to "stable" if empty)
	if stability == "" {
		stability = "stable"
	}
	deviceInfo.Stability = stability

	// Update the appropriate latest version
	if isNightly {
		deviceInfo.LatestNightly = version
	} else {
		deviceInfo.Latest = version
	}

	masterManifest.Devices[deviceType] = deviceInfo

	// Write back to bucket
	logger.Info("Writing master manifest back to bucket")
	w := obj.NewWriter(ctx)

	// Marshal to JSON with indentation
	content, err := json.MarshalIndent(masterManifest, "", "  ")
	if err != nil {
		logger.WithError(err).Error("Failed to marshal master manifest")
		return fmt.Errorf("failed to marshal master manifest: %w", err)
	}

	// Write content
	if _, err := w.Write(content); err != nil {
		w.Close() // Close without checking error since write already failed
		logger.WithError(err).Error("Failed to write master manifest")
		return fmt.Errorf("failed to write master manifest: %w", err)
	}

	// Close to commit the write (MUST check error - this is when upload finalizes)
	if err := w.Close(); err != nil {
		logger.WithError(err).Error("Failed to finalize master manifest write")
		return fmt.Errorf("failed to finalize master manifest write: %w", err)
	}

	logger.Info("Successfully wrote master manifest")
	return nil
}

// createNewDevice creates a new device type in both manifests
func createNewDevice(ctx context.Context, bucket *storage.BucketHandle, deviceType string, stability string) error {
	logger := log.WithFields(logrus.Fields{
		"device_type": deviceType,
		"stability":   stability,
	})
	logger.Info("Creating new device type")

	// Create empty device manifest
	manifestPath := fmt.Sprintf("manifests/%s.json", deviceType)
	manifest := DeviceManifest{
		DeviceID: deviceType,
		Versions: make(map[string]VersionMetadata),
	}

	// Write device manifest
	obj := bucket.Object(manifestPath)
	w := obj.NewWriter(ctx)

	content, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		logger.WithError(err).Error("Failed to marshal device manifest")
		return fmt.Errorf("failed to marshal device manifest: %w", err)
	}

	if _, err := w.Write(content); err != nil {
		w.Close() // Close without checking error since write already failed
		logger.WithError(err).Error("Failed to write device manifest")
		return fmt.Errorf("failed to write device manifest: %w", err)
	}

	// Close to commit the write (MUST check error - this is when upload finalizes)
	if err := w.Close(); err != nil {
		logger.WithError(err).Error("Failed to finalize device manifest write")
		return fmt.Errorf("failed to finalize device manifest write: %w", err)
	}

	logger.Info("Successfully created device manifest")

	// Update master manifest to include the new device
	masterManifestPath := "manifests/master.json"
	masterObj := bucket.Object(masterManifestPath)

	var masterManifest MasterManifest
	r, err := masterObj.NewReader(ctx)
	if err == nil {
		defer r.Close()
		content, err := io.ReadAll(r)
		if err != nil {
			logger.WithError(err).Error("Failed to read master manifest")
			return fmt.Errorf("failed to read master manifest: %w", err)
		}

		if err := json.Unmarshal(content, &masterManifest); err != nil {
			logger.WithError(err).Error("Failed to decode master manifest")
			return fmt.Errorf("failed to decode master manifest: %w", err)
		}
	} else {
		masterManifest = MasterManifest{
			Devices: make(map[string]DeviceLatestInfo),
		}
	}

	// Add the new device to master manifest
	masterManifest.LastUpdated = time.Now()

	// Set stability (defaults to "stable" if empty)
	if stability == "" {
		stability = "stable"
	}

	masterManifest.Devices[deviceType] = DeviceLatestInfo{
		Latest:       "",
		ManifestPath: manifestPath,
		Stability:    stability,
	}

	// Write master manifest
	w = masterObj.NewWriter(ctx)

	content, err = json.MarshalIndent(masterManifest, "", "  ")
	if err != nil {
		logger.WithError(err).Error("Failed to marshal master manifest")
		return fmt.Errorf("failed to marshal master manifest: %w", err)
	}

	if _, err := w.Write(content); err != nil {
		w.Close() // Close without checking error since write already failed
		logger.WithError(err).Error("Failed to write master manifest")
		return fmt.Errorf("failed to write master manifest: %w", err)
	}

	// Close to commit the write (MUST check error - this is when upload finalizes)
	if err := w.Close(); err != nil {
		logger.WithError(err).Error("Failed to finalize master manifest write")
		return fmt.Errorf("failed to finalize master manifest write: %w", err)
	}

	logger.Info("Successfully updated master manifest")
	return nil
}
