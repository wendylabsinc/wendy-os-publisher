package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"cloud.google.com/go/storage"
	"github.com/sirupsen/logrus"
	"google.golang.org/api/iterator"
)

var log = logrus.New()

// DeviceManifest represents a device-specific manifest
type DeviceManifest struct {
	DeviceID string                     `json:"device_id"`
	Versions map[string]VersionMetadata `json:"versions"`
}

// VersionMetadata contains metadata about a specific OS version
type VersionMetadata struct {
	ReleaseDate       time.Time  `json:"release_date"`
	Path              string     `json:"path"`
	Checksum          string     `json:"checksum,omitempty"`
	SizeBytes         int64      `json:"size_bytes"`
	Changelog         string     `json:"changelog,omitempty"`
	IsLatest          bool       `json:"is_latest"`
	IsNightly         bool       `json:"is_nightly,omitempty"`
	PromotedFrom      *string    `json:"promoted_from,omitempty"`      // Source nightly version
	PromotedAt        *time.Time `json:"promoted_at,omitempty"`        // Promotion timestamp
	SwappedAt         *time.Time `json:"swapped_at,omitempty"`         // Last swap timestamp
	SwapCount         *int       `json:"swap_count,omitempty"`         // Number of times swapped
	RecoveryPath      *string    `json:"recovery_path,omitempty"`      // Optional recovery/tegraflash file path
	RecoveryChecksum  *string    `json:"recovery_checksum,omitempty"`  // Recovery file checksum
	RecoverySizeBytes *int64     `json:"recovery_size_bytes,omitempty"` // Recovery file size
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

// ProgressReader wraps an io.Reader and reports progress
type ProgressReader struct {
	reader      io.Reader
	total       int64
	read        int64
	lastPercent int
	callback    func(read int64, total int64, percent int)
}

// NewProgressReader creates a new progress tracking reader
func NewProgressReader(r io.Reader, total int64, callback func(int64, int64, int)) *ProgressReader {
	return &ProgressReader{
		reader:   r,
		total:    total,
		callback: callback,
	}
}

// Read implements io.Reader interface
func (pr *ProgressReader) Read(p []byte) (int, error) {
	n, err := pr.reader.Read(p)
	pr.read += int64(n)

	// Calculate percentage
	percent := int(float64(pr.read) / float64(pr.total) * 100)

	// Only report if percentage changed (avoid spam)
	if percent != pr.lastPercent {
		pr.lastPercent = percent
		if pr.callback != nil {
			pr.callback(pr.read, pr.total, percent)
		}
	}

	return n, err
}

// printProgress displays upload progress to stdout
func printProgress(read int64, total int64, percent int) {
	// Convert to human-readable format
	readMB := float64(read) / (1024 * 1024)
	totalMB := float64(total) / (1024 * 1024)

	// Create progress bar (50 chars wide)
	barWidth := 50
	filled := int(float64(barWidth) * float64(percent) / 100)
	bar := strings.Repeat("=", filled) + strings.Repeat("-", barWidth-filled)

	// Print with carriage return to overwrite previous line
	fmt.Printf("\rUploading: [%s] %d%% (%.2f MB / %.2f MB)", bar, percent, readMB, totalMB)

	// Add newline when complete
	if percent >= 100 {
		fmt.Println()
	}
}

// isOSImage checks if a file is an OS image based on its extension
func isOSImage(filename string) bool {
	ext := strings.ToLower(filepath.Ext(filename))
	result := ext == ".img" || ext == ".zip" || ext == ".tgz"
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
	if len(deviceType) > 100 {
		return fmt.Errorf("device type is too long (max 100 characters)")
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
	if len(version) > 100 {
		return fmt.Errorf("version is too long (max 100 characters)")
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

// createStorageClientWithAuth creates a storage client and triggers authentication if needed
func createStorageClientWithAuth(ctx context.Context) (*storage.Client, error) {
	// Try to create the client
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
	recoveryFile := flag.String("recovery-file", "", "Optional recovery/tegraflash file path")
	updateOnly := flag.Bool("update-only", false, "Only update manifests without uploading")
	listImages := flag.Bool("list", false, "List all images in the bucket")
	createDevice := flag.Bool("create-device", false, "Create a new device type in the manifest")
	nightly := flag.Bool("nightly", false, "Mark this build as a nightly/untested build")
	stability := flag.String("stability", "stable", "Device stability level: stable, experimental, deprecated")
	debug := flag.Bool("debug", false, "Enable debug logging")
	showProgress := flag.Bool("progress", true, "Show upload progress")
	promote := flag.Bool("promote", false, "Promote nightly to stable by removing 'nightly' from version name")
	swap := flag.Bool("swap", false, "Replace existing version's image file while preserving metadata")
	flag.Parse()

	if *debug {
		log.SetLevel(logrus.DebugLevel)
	}

	// Note: showProgress is parsed but currently progress is always shown
	// Can be used in the future to conditionally disable progress
	_ = showProgress

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
	} else if *promote {
		// For promotion, we need device type and version
		if err := validateDeviceType(*deviceType); err != nil {
			log.WithError(err).Fatal("Invalid device type")
		}
		if err := validateVersion(*version); err != nil {
			log.WithError(err).Fatal("Invalid source version")
		}
	} else if *swap {
		// For swap, we need device type, version, and file
		if err := validateDeviceType(*deviceType); err != nil {
			log.WithError(err).Fatal("Invalid device type")
		}
		if err := validateVersion(*version); err != nil {
			log.WithError(err).Fatal("Invalid version")
		}
		if err := validateFileExists(*localFile); err != nil {
			log.WithError(err).Fatal("Invalid file")
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
			if err := validateFileExists(*localFile); err != nil {
				log.WithError(err).Fatal("Invalid file")
			}
			// Validate recovery file if provided
			if *recoveryFile != "" {
				if err := validateFileExists(*recoveryFile); err != nil {
					log.WithError(err).Fatal("Invalid recovery file")
				}
			}
		}
	}

	// Create context and storage client
	ctx := context.Background()
	client, err := createStorageClientWithAuth(ctx)
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
		createNewDevice(ctx, bucket, *deviceType, *stability)
		return
	}

	// Promote nightly to stable if requested
	if *promote {
		promoteNightlyToStable(ctx, bucket, *deviceType, *version)
		return
	}

	// Swap image file if requested
	if *swap {
		swapImageFile(ctx, bucket, *deviceType, *version, *localFile, *nightly)
		return
	}

	// Process the request
	if !*updateOnly {
		// Upload the main image file
		destinationPath := uploadFile(ctx, bucket, *localFile, *deviceType, *version)
		if destinationPath == "" {
			return // Error already logged
		}

		// Get file size after upload
		obj := bucket.Object(destinationPath)
		attrs, err := obj.Attrs(ctx)
		if err != nil {
			log.WithError(err).Error("Failed to get file attributes")
			return
		}

		// Upload recovery file if provided
		var recoveryPath *string
		var recoverySize *int64
		if *recoveryFile != "" {
			recoveryDest := uploadFile(ctx, bucket, *recoveryFile, *deviceType, *version)
			if recoveryDest == "" {
				return // Error already logged
			}
			recoveryPath = &recoveryDest

			recoveryObj := bucket.Object(recoveryDest)
			recoveryAttrs, err := recoveryObj.Attrs(ctx)
			if err != nil {
				log.WithError(err).Error("Failed to get recovery file attributes")
				return
			}
			recoverySize = &recoveryAttrs.Size
		}

		// Update manifests with both files
		updateManifests(ctx, bucket, *deviceType, *version, destinationPath, attrs.Size, recoveryPath, recoverySize, *nightly, *stability)
	} else {
		// Just update manifests for existing file
		imagePath := fmt.Sprintf("images/%s/%s/%s", *deviceType, *version, filepath.Base(*localFile))

		obj := bucket.Object(imagePath)
		attrs, err := obj.Attrs(ctx)
		if err != nil {
			log.WithError(err).Error("Failed to get file attributes, does it exist?")
			return
		}

		// Handle recovery file in update-only mode
		var recoveryPath *string
		var recoverySize *int64
		if *recoveryFile != "" {
			recoveryImagePath := fmt.Sprintf("images/%s/%s/%s", *deviceType, *version, filepath.Base(*recoveryFile))
			recoveryPath = &recoveryImagePath

			recoveryObj := bucket.Object(recoveryImagePath)
			recoveryAttrs, err := recoveryObj.Attrs(ctx)
			if err != nil {
				log.WithError(err).Error("Failed to get recovery file attributes, does it exist?")
				return
			}
			recoverySize = &recoveryAttrs.Size
		}

		updateManifests(ctx, bucket, *deviceType, *version, imagePath, attrs.Size, recoveryPath, recoverySize, *nightly, *stability)
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

func uploadFile(ctx context.Context, bucket *storage.BucketHandle, localPath, deviceType, version string) string {
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
		return ""
	}
	defer file.Close()

	// Get file size for progress tracking
	fileInfo, err := file.Stat()
	if err != nil {
		log.WithError(err).Error("Failed to get file info")
		return ""
	}
	fileSize := fileInfo.Size()

	log.WithField("size_mb", float64(fileSize)/(1024*1024)).Info("Starting upload")

	// Create the destination object
	obj := bucket.Object(destinationPath)
	w := obj.NewWriter(ctx)

	// Set content type based on extension
	contentType := "application/octet-stream"
	if strings.HasSuffix(localPath, ".zip") {
		contentType = "application/zip"
	} else if strings.HasSuffix(localPath, ".tgz") {
		contentType = "application/gzip"
	}
	w.ContentType = contentType

	// Configure chunk size for optimal performance
	// Use 16MB chunks (default), or 8MB for smaller files
	if fileSize < 100*1024*1024 { // Less than 100MB
		w.ChunkSize = 8 * 1024 * 1024 // 8MB chunks
	} else {
		w.ChunkSize = 16 * 1024 * 1024 // 16MB chunks
	}

	// Create progress reader wrapper
	progressReader := NewProgressReader(file, fileSize, printProgress)

	// Stream upload with progress tracking
	bytesWritten, err := io.Copy(w, progressReader)
	if err != nil {
		fmt.Println() // Clear the progress line
		log.WithError(err).Error("Failed to upload to GCS")
		w.Close() // Close writer to cleanup
		return ""
	}

	// Close to commit the write
	if err := w.Close(); err != nil {
		fmt.Println() // Clear the progress line
		log.WithError(err).Error("Failed to close GCS writer")
		return ""
	}

	log.WithFields(logrus.Fields{
		"path":    destinationPath,
		"bytes":   bytesWritten,
		"size_mb": float64(bytesWritten) / (1024 * 1024),
	}).Info("File uploaded successfully")

	return destinationPath
}

// calculateFileChecksum calculates SHA256 checksum for a file in GCS
func calculateFileChecksum(ctx context.Context, bucket *storage.BucketHandle, filePath string, logger *logrus.Entry) string {
	obj := bucket.Object(filePath)
	r, err := obj.NewReader(ctx)
	if err != nil {
		logger.WithError(err).Error("Failed to read file for checksum calculation")
		return ""
	}
	defer r.Close()

	hash := sha256.New()
	if _, err := io.Copy(hash, r); err != nil {
		logger.WithError(err).Error("Failed to calculate checksum")
		return ""
	}

	checksum := fmt.Sprintf("%x", hash.Sum(nil))
	logger.WithField("checksum", checksum).Debug("Calculated checksum")
	return checksum
}

func updateManifests(ctx context.Context, bucket *storage.BucketHandle, deviceType, version, filePath string, fileSize int64, recoveryPath *string, recoverySize *int64, isNightly bool, stability string) {
	logger := log.WithFields(logrus.Fields{
		"device_type":    deviceType,
		"version":        version,
		"file_path":      filePath,
		"recovery_path":  recoveryPath,
		"is_nightly":     isNightly,
		"stability":      stability,
	})
	logger.Info("Updating manifests")

	// Update device manifest
	updateDeviceManifest(ctx, logger, bucket, deviceType, version, filePath, fileSize, recoveryPath, recoverySize, isNightly)

	// Update master manifest
	updateMasterManifest(ctx, logger, bucket, deviceType, version, isNightly, stability)
}

func updateDeviceManifest(ctx context.Context, logger *logrus.Entry, bucket *storage.BucketHandle, deviceType, version, filePath string, fileSize int64, recoveryPath *string, recoverySize *int64, isNightly bool) {
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

		// Read content
		content, err := ioutil.ReadAll(r)
		if err != nil {
			logger.WithError(err).Error("Failed to read existing device manifest")
			return
		}

		// Unmarshal JSON
		if err := json.Unmarshal(content, &manifest); err != nil {
			logger.WithError(err).Error("Failed to decode existing device manifest")
			return
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
			errMsg := fmt.Sprintf("version %s already exists as %s build, cannot change to %s build",
				version,
				map[bool]string{true: "nightly", false: "stable"}[existingVersion.IsNightly],
				map[bool]string{true: "nightly", false: "stable"}[isNightly])
			logger.Error(errMsg)
			return
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

	// Calculate checksum for main image file
	logger.Info("Calculating checksum for main image file")
	imageChecksum := calculateFileChecksum(ctx, bucket, filePath, logger)

	// Calculate checksum for recovery file if provided
	var recoveryChecksum *string
	if recoveryPath != nil {
		logger.Info("Calculating checksum for recovery file")
		checksumValue := calculateFileChecksum(ctx, bucket, *recoveryPath, logger)
		recoveryChecksum = &checksumValue
	}

	// Add or update this version and mark as latest
	manifest.Versions[version] = VersionMetadata{
		ReleaseDate:       time.Now(),
		Path:              filePath,
		Checksum:          imageChecksum,
		SizeBytes:         fileSize,
		IsLatest:          true,
		IsNightly:         isNightly,
		RecoveryPath:      recoveryPath,
		RecoveryChecksum:  recoveryChecksum,
		RecoverySizeBytes: recoverySize,
	}

	// Write back to bucket
	logger.Info("Writing device manifest back to bucket")
	w := obj.NewWriter(ctx)
	defer w.Close()

	// Marshal to JSON with indentation
	content, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		logger.WithError(err).Error("Failed to marshal device manifest")
		return
	}

	// Write content
	if _, err := w.Write(content); err != nil {
		logger.WithError(err).Error("Failed to write device manifest")
		return
	}

	logger.Info("Successfully wrote device manifest")
}

// swapUpdateDeviceManifest updates device manifest for a swap operation, preserving historical metadata
func swapUpdateDeviceManifest(ctx context.Context, logger *logrus.Entry, bucket *storage.BucketHandle, deviceType, version, newPath, newChecksum string, newSize int64, existingMeta VersionMetadata) error {
	manifestPath := fmt.Sprintf("manifests/%s.json", deviceType)
	logger = logger.WithField("manifest_path", manifestPath)
	logger.Info("Updating device manifest for swap operation")

	obj := bucket.Object(manifestPath)

	// Read existing manifest
	var manifest DeviceManifest
	r, err := obj.NewReader(ctx)
	if err != nil {
		return fmt.Errorf("failed to read device manifest: %w", err)
	}
	defer r.Close()

	content, err := ioutil.ReadAll(r)
	if err != nil {
		return fmt.Errorf("failed to read manifest content: %w", err)
	}

	if err := json.Unmarshal(content, &manifest); err != nil {
		return fmt.Errorf("failed to parse device manifest: %w", err)
	}

	// Get current version to extract existing metadata
	currentVersion, exists := manifest.Versions[version]
	if !exists {
		return fmt.Errorf("version %s not found in manifest", version)
	}

	// Increment swap counter
	swapCount := 1
	if currentVersion.SwapCount != nil {
		swapCount = *currentVersion.SwapCount + 1
	}
	swappedAt := time.Now()

	// Build new metadata preserving historical fields
	updatedVersion := VersionMetadata{
		// Preserved fields
		ReleaseDate:  currentVersion.ReleaseDate, // Original upload timestamp
		Changelog:    currentVersion.Changelog,
		IsLatest:     currentVersion.IsLatest,
		IsNightly:    currentVersion.IsNightly,
		PromotedFrom: currentVersion.PromotedFrom,
		PromotedAt:   currentVersion.PromotedAt,

		// Preserved recovery file fields (swap only affects main image)
		RecoveryPath:      currentVersion.RecoveryPath,
		RecoveryChecksum:  currentVersion.RecoveryChecksum,
		RecoverySizeBytes: currentVersion.RecoverySizeBytes,

		// Updated fields
		Path:      newPath,
		SizeBytes: newSize,
		Checksum:  newChecksum,
		SwappedAt: &swappedAt,
		SwapCount: &swapCount,
	}

	// Update manifest
	manifest.Versions[version] = updatedVersion

	// Write back to bucket
	logger.WithFields(logrus.Fields{
		"swap_count": swapCount,
		"new_path":   newPath,
	}).Info("Writing updated device manifest")

	w := obj.NewWriter(ctx)
	defer w.Close()

	manifestContent, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal device manifest: %w", err)
	}

	if _, err := w.Write(manifestContent); err != nil {
		return fmt.Errorf("failed to write device manifest: %w", err)
	}

	logger.Info("Successfully updated device manifest for swap")
	return nil
}

func updateMasterManifest(ctx context.Context, logger *logrus.Entry, bucket *storage.BucketHandle, deviceType, version string, isNightly bool, stability string) {
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

		// Read content
		content, err := ioutil.ReadAll(r)
		if err != nil {
			logger.WithError(err).Error("Failed to read existing master manifest")
			return
		}

		// Unmarshal JSON
		if err := json.Unmarshal(content, &masterManifest); err != nil {
			logger.WithError(err).Error("Failed to decode existing master manifest")
			return
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
	defer w.Close()

	// Marshal to JSON with indentation
	content, err := json.MarshalIndent(masterManifest, "", "  ")
	if err != nil {
		logger.WithError(err).Error("Failed to marshal master manifest")
		return
	}

	// Write content
	if _, err := w.Write(content); err != nil {
		logger.WithError(err).Error("Failed to write master manifest")
		return
	}

	logger.Info("Successfully wrote master manifest")
}

// createNewDevice creates a new device type in both manifests
func createNewDevice(ctx context.Context, bucket *storage.BucketHandle, deviceType string, stability string) {
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
	defer w.Close()

	content, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		logger.WithError(err).Fatal("Failed to marshal device manifest")
	}

	if _, err := w.Write(content); err != nil {
		logger.WithError(err).Fatal("Failed to write device manifest")
	}

	logger.Info("Successfully created device manifest")

	// Update master manifest to include the new device
	masterManifestPath := "manifests/master.json"
	masterObj := bucket.Object(masterManifestPath)

	var masterManifest MasterManifest
	r, err := masterObj.NewReader(ctx)
	if err == nil {
		defer r.Close()
		content, err := ioutil.ReadAll(r)
		if err != nil {
			logger.WithError(err).Fatal("Failed to read master manifest")
		}

		if err := json.Unmarshal(content, &masterManifest); err != nil {
			logger.WithError(err).Fatal("Failed to decode master manifest")
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
	defer w.Close()

	content, err = json.MarshalIndent(masterManifest, "", "  ")
	if err != nil {
		logger.WithError(err).Fatal("Failed to marshal master manifest")
	}

	if _, err := w.Write(content); err != nil {
		logger.WithError(err).Fatal("Failed to write master manifest")
	}

	logger.Info("Successfully updated master manifest")
}

// promoteNightlyToStable promotes a nightly build to stable release by removing "nightly" from version name
func promoteNightlyToStable(ctx context.Context, bucket *storage.BucketHandle, deviceType, nightlyVersion string) {
	logger := log.WithFields(logrus.Fields{
		"device_type":     deviceType,
		"nightly_version": nightlyVersion,
		"operation":       "promote",
	})
	logger.Info("Promoting nightly build to stable")

	// Transform version name (remove "nightly-" prefix or "-nightly" suffix)
	stableVersion := nightlyVersion
	if strings.HasPrefix(stableVersion, "nightly-") {
		stableVersion = strings.TrimPrefix(stableVersion, "nightly-")
	} else if strings.HasSuffix(stableVersion, "-nightly") {
		stableVersion = strings.TrimSuffix(stableVersion, "-nightly")
	} else if stableVersion == "nightly" {
		logger.Fatal("Cannot promote version 'nightly' - must have additional version info (e.g., 'nightly-2025-12-20')")
	} else if !strings.Contains(strings.ToLower(stableVersion), "nightly") {
		logger.Fatal("Version does not contain 'nightly' - cannot determine stable version name")
	}

	logger.WithField("stable_version", stableVersion).Info("Derived stable version name")

	// Read device manifest
	manifestPath := fmt.Sprintf("manifests/%s.json", deviceType)
	obj := bucket.Object(manifestPath)

	var manifest DeviceManifest
	r, err := obj.NewReader(ctx)
	if err != nil {
		logger.WithError(err).Fatal("Failed to read device manifest - does the device exist?")
	}
	defer r.Close()

	content, err := ioutil.ReadAll(r)
	if err != nil {
		logger.WithError(err).Fatal("Failed to read manifest content")
	}

	if err := json.Unmarshal(content, &manifest); err != nil {
		logger.WithError(err).Fatal("Failed to parse device manifest")
	}

	// Validate source version exists and is nightly
	sourceVersionMeta, exists := manifest.Versions[nightlyVersion]
	if !exists {
		logger.Fatal("Source version does not exist in manifest")
	}

	if !sourceVersionMeta.IsNightly {
		logger.Fatal("Source version is not a nightly build - cannot promote")
	}

	// Validate target version doesn't already exist
	if _, exists := manifest.Versions[stableVersion]; exists {
		logger.WithField("stable_version", stableVersion).Fatal("Target stable version already exists")
	}

	// Get source file path and parse to create destination path
	sourcePath := sourceVersionMeta.Path
	// Path format: images/{device}/{version}/{filename}
	parts := strings.Split(sourcePath, "/")
	if len(parts) < 4 {
		logger.WithField("path", sourcePath).Fatal("Invalid source file path format")
	}
	filename := parts[len(parts)-1]
	destinationPath := fmt.Sprintf("images/%s/%s/%s", deviceType, stableVersion, filename)

	logger.WithFields(logrus.Fields{
		"source_path":      sourcePath,
		"destination_path": destinationPath,
	}).Info("Copying file to stable path")

	// Copy GCS file to new stable path using server-side copy
	srcObj := bucket.Object(sourcePath)
	dstObj := bucket.Object(destinationPath)
	copier := dstObj.CopierFrom(srcObj)

	attrs, err := copier.Run(ctx)
	if err != nil {
		logger.WithError(err).Fatal("Failed to copy file to stable path")
	}

	logger.WithField("size_bytes", attrs.Size).Info("File copied successfully")

	// Copy recovery file if it exists
	var recoveryDestPath *string
	var recoveryChecksum *string
	var recoverySizeBytes *int64
	if sourceVersionMeta.RecoveryPath != nil {
		// Parse recovery source path
		recoverySourcePath := *sourceVersionMeta.RecoveryPath
		recoveryParts := strings.Split(recoverySourcePath, "/")
		if len(recoveryParts) < 4 {
			logger.WithField("path", recoverySourcePath).Warn("Invalid recovery file path format, skipping")
		} else {
			recoveryFilename := recoveryParts[len(recoveryParts)-1]
			recoveryDest := fmt.Sprintf("images/%s/%s/%s", deviceType, stableVersion, recoveryFilename)

			logger.WithFields(logrus.Fields{
				"source_path":      recoverySourcePath,
				"destination_path": recoveryDest,
			}).Info("Copying recovery file to stable path")

			// Copy recovery file using server-side copy
			recoverySrcObj := bucket.Object(recoverySourcePath)
			recoveryDstObj := bucket.Object(recoveryDest)
			recoveryCopier := recoveryDstObj.CopierFrom(recoverySrcObj)

			recoveryAttrs, err := recoveryCopier.Run(ctx)
			if err != nil {
				logger.WithError(err).Warn("Failed to copy recovery file, continuing without it")
			} else {
				logger.WithField("size_bytes", recoveryAttrs.Size).Info("Recovery file copied successfully")
				recoveryDestPath = &recoveryDest
				recoveryChecksum = sourceVersionMeta.RecoveryChecksum
				recoverySizeBytes = sourceVersionMeta.RecoverySizeBytes
			}
		}
	}

	// Create new stable version entry with promotion metadata
	promotedAt := time.Now()
	sourceVersion := nightlyVersion
	stableVersionMeta := VersionMetadata{
		ReleaseDate:       sourceVersionMeta.ReleaseDate, // Preserve original release date
		Path:              destinationPath,
		Checksum:          sourceVersionMeta.Checksum,
		SizeBytes:         sourceVersionMeta.SizeBytes,
		Changelog:         sourceVersionMeta.Changelog,
		IsLatest:          true,
		IsNightly:         false,
		PromotedFrom:      &sourceVersion,
		PromotedAt:        &promotedAt,
		RecoveryPath:      recoveryDestPath,
		RecoveryChecksum:  recoveryChecksum,
		RecoverySizeBytes: recoverySizeBytes,
	}

	// Update IsLatest flags - clear all stable versions
	for k, v := range manifest.Versions {
		if !v.IsNightly {
			v.IsLatest = false
			manifest.Versions[k] = v
		}
	}

	// Add new stable version
	manifest.Versions[stableVersion] = stableVersionMeta

	// Write device manifest back
	logger.Info("Writing updated device manifest")
	w := obj.NewWriter(ctx)
	defer w.Close()

	manifestContent, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		logger.WithError(err).Fatal("Failed to marshal device manifest")
	}

	if _, err := w.Write(manifestContent); err != nil {
		logger.WithError(err).Fatal("Failed to write device manifest")
	}

	logger.Info("Successfully updated device manifest")

	// Update master manifest
	updateMasterManifestForPromotion(ctx, logger, bucket, deviceType, stableVersion)

	logger.WithField("stable_version", stableVersion).Info("Successfully promoted nightly to stable")
}

// updateMasterManifestForPromotion updates master manifest after promotion
func updateMasterManifestForPromotion(ctx context.Context, logger *logrus.Entry, bucket *storage.BucketHandle, deviceType, stableVersion string) {
	masterManifestPath := "manifests/master.json"
	obj := bucket.Object(masterManifestPath)

	var masterManifest MasterManifest
	r, err := obj.NewReader(ctx)
	if err != nil {
		logger.WithError(err).Fatal("Failed to read master manifest")
	}
	defer r.Close()

	content, err := ioutil.ReadAll(r)
	if err != nil {
		logger.WithError(err).Fatal("Failed to read master manifest content")
	}

	if err := json.Unmarshal(content, &masterManifest); err != nil {
		logger.WithError(err).Fatal("Failed to decode master manifest")
	}

	masterManifest.LastUpdated = time.Now()

	deviceInfo, exists := masterManifest.Devices[deviceType]
	if !exists {
		logger.Fatal("Device does not exist in master manifest")
	}

	// Update to reflect promotion
	deviceInfo.Latest = stableVersion

	masterManifest.Devices[deviceType] = deviceInfo

	// Write back
	w := obj.NewWriter(ctx)
	defer w.Close()

	manifestContent, err := json.MarshalIndent(masterManifest, "", "  ")
	if err != nil {
		logger.WithError(err).Fatal("Failed to marshal master manifest")
	}

	if _, err := w.Write(manifestContent); err != nil {
		logger.WithError(err).Fatal("Failed to write master manifest")
	}

	logger.Info("Successfully updated master manifest")
}

// swapImageFile replaces an existing version's image file while preserving metadata
func swapImageFile(ctx context.Context, bucket *storage.BucketHandle, deviceType, version, localFile string, isNightly bool) {
	logger := log.WithFields(logrus.Fields{
		"device_type": deviceType,
		"version":     version,
		"local_file":  localFile,
		"is_nightly":  isNightly,
		"operation":   "swap",
	})
	logger.Info("Starting image file swap")

	// Step 1: Read device manifest to validate version exists
	manifestPath := fmt.Sprintf("manifests/%s.json", deviceType)
	obj := bucket.Object(manifestPath)

	var manifest DeviceManifest
	r, err := obj.NewReader(ctx)
	if err != nil {
		logger.WithError(err).Fatal("Failed to read device manifest - does the device exist?")
	}
	defer r.Close()

	content, err := ioutil.ReadAll(r)
	if err != nil {
		logger.WithError(err).Fatal("Failed to read manifest content")
	}

	if err := json.Unmarshal(content, &manifest); err != nil {
		logger.WithError(err).Fatal("Failed to parse device manifest")
	}

	// Step 2: Validate version exists
	existingVersion, exists := manifest.Versions[version]
	if !exists {
		logger.WithField("version", version).Fatal("Cannot swap - version does not exist. Use normal upload to create new version.")
	}

	logger.WithFields(logrus.Fields{
		"existing_path":       existingVersion.Path,
		"existing_is_nightly": existingVersion.IsNightly,
	}).Info("Found existing version")

	// Step 3: Verify IsNightly flag matches
	if existingVersion.IsNightly != isNightly {
		logger.WithFields(logrus.Fields{
			"existing_is_nightly":  existingVersion.IsNightly,
			"requested_is_nightly": isNightly,
		}).Fatal("Cannot swap - IsNightly flag mismatch. Version is " +
			map[bool]string{true: "nightly", false: "stable"}[existingVersion.IsNightly] +
			" but --nightly flag is " +
			map[bool]string{true: "set", false: "not set"}[isNightly])
	}

	// Step 4: Upload new file
	filename := filepath.Base(localFile)
	destinationPath := fmt.Sprintf("images/%s/%s/%s", deviceType, version, filename)

	// Warn if filename changed
	oldFilename := filepath.Base(existingVersion.Path)
	if filename != oldFilename {
		logger.WithFields(logrus.Fields{
			"old_filename": oldFilename,
			"new_filename": filename,
		}).Warn("Filename changed during swap - old file will be orphaned")
	}

	logger.WithFields(logrus.Fields{
		"local_path":  localFile,
		"destination": destinationPath,
	}).Info("Uploading new file")

	// Open the local file
	file, err := os.Open(localFile)
	if err != nil {
		logger.WithError(err).Fatal("Failed to open local file")
	}
	defer file.Close()

	// Get file size for progress tracking
	fileInfo, err := file.Stat()
	if err != nil {
		logger.WithError(err).Fatal("Failed to get file info")
	}
	fileSize := fileInfo.Size()

	logger.WithField("size_mb", float64(fileSize)/(1024*1024)).Info("Starting upload")

	// Create the destination object
	destObj := bucket.Object(destinationPath)
	w := destObj.NewWriter(ctx)

	// Set content type based on extension
	contentType := "application/octet-stream"
	if strings.HasSuffix(localFile, ".zip") {
		contentType = "application/zip"
	} else if strings.HasSuffix(localFile, ".tgz") {
		contentType = "application/gzip"
	}
	w.ContentType = contentType

	// Configure chunk size for optimal performance
	if fileSize < 100*1024*1024 {
		w.ChunkSize = 8 * 1024 * 1024
	} else {
		w.ChunkSize = 16 * 1024 * 1024
	}

	// Create progress reader wrapper
	progressReader := NewProgressReader(file, fileSize, printProgress)

	// Stream upload with progress tracking
	bytesWritten, err := io.Copy(w, progressReader)
	if err != nil {
		fmt.Println()
		logger.WithError(err).Fatal("Failed to upload to GCS")
	}

	// Close to commit the write
	if err := w.Close(); err != nil {
		fmt.Println()
		logger.WithError(err).Fatal("Failed to close GCS writer")
	}

	logger.WithFields(logrus.Fields{
		"path":    destinationPath,
		"bytes":   bytesWritten,
		"size_mb": float64(bytesWritten) / (1024 * 1024),
	}).Info("File uploaded successfully")

	// Step 5: Calculate SHA256 checksum of uploaded file
	logger.Info("Calculating checksum of uploaded file")

	checksumObj := bucket.Object(destinationPath)
	checksumReader, err := checksumObj.NewReader(ctx)
	if err != nil {
		logger.WithError(err).Fatal("Failed to read uploaded file for checksum")
	}
	defer checksumReader.Close()

	hash := sha256.New()
	if _, err := io.Copy(hash, checksumReader); err != nil {
		logger.WithError(err).Fatal("Failed to calculate checksum")
	}
	checksum := fmt.Sprintf("%x", hash.Sum(nil))

	logger.WithField("checksum", checksum).Info("Checksum calculated")

	// Step 6: Update device manifest with preserved metadata
	if err := swapUpdateDeviceManifest(ctx, logger, bucket, deviceType, version, destinationPath, checksum, bytesWritten, existingVersion); err != nil {
		logger.WithError(err).Fatal("Failed to update device manifest")
	}

	// Step 7: Update master manifest (timestamp only, no version change)
	updateMasterManifestTimestamp(ctx, logger, bucket)

	logger.WithFields(logrus.Fields{
		"version":      version,
		"new_path":     destinationPath,
		"new_checksum": checksum,
		"new_size":     bytesWritten,
	}).Info("Successfully swapped image file")
}

// updateMasterManifestTimestamp updates only the timestamp in master manifest
func updateMasterManifestTimestamp(ctx context.Context, logger *logrus.Entry, bucket *storage.BucketHandle) {
	masterManifestPath := "manifests/master.json"
	obj := bucket.Object(masterManifestPath)

	var masterManifest MasterManifest
	r, err := obj.NewReader(ctx)
	if err != nil {
		logger.WithError(err).Fatal("Failed to read master manifest")
	}
	defer r.Close()

	content, err := ioutil.ReadAll(r)
	if err != nil {
		logger.WithError(err).Fatal("Failed to read master manifest content")
	}

	if err := json.Unmarshal(content, &masterManifest); err != nil {
		logger.WithError(err).Fatal("Failed to decode master manifest")
	}

	// Update only the timestamp
	masterManifest.LastUpdated = time.Now()

	// Write back
	w := obj.NewWriter(ctx)
	defer w.Close()

	manifestContent, err := json.MarshalIndent(masterManifest, "", "  ")
	if err != nil {
		logger.WithError(err).Fatal("Failed to marshal master manifest")
	}

	if _, err := w.Write(manifestContent); err != nil {
		logger.WithError(err).Fatal("Failed to write master manifest")
	}

	logger.Info("Updated master manifest timestamp")
}
