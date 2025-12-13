package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
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
	ReleaseDate time.Time `json:"release_date"`
	Path        string    `json:"path"`
	Checksum    string    `json:"checksum,omitempty"`
	SizeBytes   int64     `json:"size_bytes"`
	Changelog   string    `json:"changelog,omitempty"`
	IsLatest    bool      `json:"is_latest"`
	IsNightly   bool      `json:"is_nightly,omitempty"`
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
	updateOnly := flag.Bool("update-only", false, "Only update manifests without uploading")
	listImages := flag.Bool("list", false, "List all images in the bucket")
	createDevice := flag.Bool("create-device", false, "Create a new device type in the manifest")
	nightly := flag.Bool("nightly", false, "Mark this build as a nightly/untested build")
	stability := flag.String("stability", "stable", "Device stability level: stable, experimental, deprecated")
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
			if err := validateFileExists(*localFile); err != nil {
				log.WithError(err).Fatal("Invalid file")
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

	// Process the request
	if !*updateOnly {
		// Upload the file
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

		// Update manifests
		updateManifests(ctx, bucket, *deviceType, *version, destinationPath, attrs.Size, *nightly, *stability)
	} else {
		// Just update manifests for existing file
		imagePath := fmt.Sprintf("images/%s/%s/%s", *deviceType, *version, filepath.Base(*localFile))

		obj := bucket.Object(imagePath)
		attrs, err := obj.Attrs(ctx)
		if err != nil {
			log.WithError(err).Error("Failed to get file attributes, does it exist?")
			return
		}

		updateManifests(ctx, bucket, *deviceType, *version, imagePath, attrs.Size, *nightly, *stability)
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

	// Read file content
	content, err := ioutil.ReadAll(file)
	if err != nil {
		log.WithError(err).Error("Failed to read local file")
		return ""
	}

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

	// Write the content
	if _, err := w.Write(content); err != nil {
		log.WithError(err).Error("Failed to write to GCS")
		return ""
	}

	// Close to commit the write
	if err := w.Close(); err != nil {
		log.WithError(err).Error("Failed to close GCS writer")
		return ""
	}

	log.WithField("path", destinationPath).Info("File uploaded successfully")
	return destinationPath
}

func updateManifests(ctx context.Context, bucket *storage.BucketHandle, deviceType, version, filePath string, fileSize int64, isNightly bool, stability string) {
	logger := log.WithFields(logrus.Fields{
		"device_type": deviceType,
		"version":     version,
		"file_path":   filePath,
		"is_nightly":  isNightly,
		"stability":   stability,
	})
	logger.Info("Updating manifests")

	// Update device manifest
	updateDeviceManifest(ctx, logger, bucket, deviceType, version, filePath, fileSize, isNightly)

	// Update master manifest
	updateMasterManifest(ctx, logger, bucket, deviceType, version, isNightly, stability)
}

func updateDeviceManifest(ctx context.Context, logger *logrus.Entry, bucket *storage.BucketHandle, deviceType, version, filePath string, fileSize int64, isNightly bool) {
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

	// Add or update this version and mark as latest
	manifest.Versions[version] = VersionMetadata{
		ReleaseDate: time.Now(),
		Path:        filePath,
		SizeBytes:   fileSize,
		IsLatest:    true,
		IsNightly:   isNightly,
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
