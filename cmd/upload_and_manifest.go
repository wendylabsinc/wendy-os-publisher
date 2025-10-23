package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io/ioutil"
	"os"
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
}

// MasterManifest represents the top-level manifest
type MasterManifest struct {
	LastUpdated time.Time                   `json:"last_updated"`
	Devices     map[string]DeviceLatestInfo `json:"devices"`
}

// DeviceLatestInfo holds info about the latest version for a device
type DeviceLatestInfo struct {
	Latest       string `json:"latest"`
	ManifestPath string `json:"manifest_path"`
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
		if *deviceType == "" {
			log.Fatal("Device type is required when creating a new device")
		}
	} else {
		// For normal operations, we need device type and version
		if *deviceType == "" || *version == "" {
			log.Fatal("Device type and version are required")
		}
		if !*updateOnly && *localFile == "" {
			log.Fatal("Local file path is required for upload")
		}
	}

	// Create context and storage client
	ctx := context.Background()
	client, err := storage.NewClient(ctx)
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
		createNewDevice(ctx, bucket, *deviceType)
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
		updateManifests(ctx, bucket, *deviceType, *version, destinationPath, attrs.Size)
	} else {
		// Just update manifests for existing file
		imagePath := fmt.Sprintf("images/%s/%s/%s", *deviceType, *version, filepath.Base(*localFile))

		obj := bucket.Object(imagePath)
		attrs, err := obj.Attrs(ctx)
		if err != nil {
			log.WithError(err).Error("Failed to get file attributes, does it exist?")
			return
		}

		updateManifests(ctx, bucket, *deviceType, *version, imagePath, attrs.Size)
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

func updateManifests(ctx context.Context, bucket *storage.BucketHandle, deviceType, version, filePath string, fileSize int64) {
	logger := log.WithFields(logrus.Fields{
		"device_type": deviceType,
		"version":     version,
		"file_path":   filePath,
	})
	logger.Info("Updating manifests")

	// Update device manifest
	updateDeviceManifest(ctx, logger, bucket, deviceType, version, filePath, fileSize)

	// Update master manifest
	updateMasterManifest(ctx, logger, bucket, deviceType, version)
}

func updateDeviceManifest(ctx context.Context, logger *logrus.Entry, bucket *storage.BucketHandle, deviceType, version, filePath string, fileSize int64) {
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

	// Update version information
	// Set all existing versions' IsLatest to false
	for k, v := range manifest.Versions {
		v.IsLatest = false
		manifest.Versions[k] = v
	}

	// Add or update this version and mark as latest
	logger.WithField("version", version).Info("Setting version as latest")
	manifest.Versions[version] = VersionMetadata{
		ReleaseDate: time.Now(),
		Path:        filePath,
		SizeBytes:   fileSize,
		IsLatest:    true,
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

func updateMasterManifest(ctx context.Context, logger *logrus.Entry, bucket *storage.BucketHandle, deviceType, version string) {
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
	}).Info("Updating master manifest")

	masterManifest.LastUpdated = time.Now()
	masterManifest.Devices[deviceType] = DeviceLatestInfo{
		Latest:       version,
		ManifestPath: fmt.Sprintf("manifests/%s.json", deviceType),
	}

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
func createNewDevice(ctx context.Context, bucket *storage.BucketHandle, deviceType string) {
	logger := log.WithField("device_type", deviceType)
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
	masterManifest.Devices[deviceType] = DeviceLatestInfo{
		Latest:       "",
		ManifestPath: manifestPath,
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
