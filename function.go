package function

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"cloud.google.com/go/storage"
	"github.com/GoogleCloudPlatform/functions-framework-go/functions"
	"github.com/cloudevents/sdk-go/v2/event"
	"github.com/sirupsen/logrus"
)

var log = logrus.New()

func init() {
	// Configure logrus
	log.SetFormatter(&logrus.JSONFormatter{})
	log.SetLevel(logrus.InfoLevel)

	// Register the Cloud Function
	functions.CloudEvent("ProcessPubSubMessage", processPubSubMessage)
}

// MessagePublishedData contains the full Pub/Sub message
type MessagePublishedData struct {
	Message PubSubMessage
}

// PubSubMessage is the payload of a Pub/Sub event
type PubSubMessage struct {
	Data       []byte            `json:"data"`
	Attributes map[string]string `json:"attributes"`
}

// StorageNotification is the payload of a GCS notification
type StorageNotification struct {
	Name        string    `json:"name"`
	Bucket      string    `json:"bucket"`
	ContentType string    `json:"contentType"`
	Size        string    `json:"size"`
	TimeCreated time.Time `json:"timeCreated"`
	Updated     time.Time `json:"updated"`
}

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

func processPubSubMessage(ctx context.Context, e event.Event) error {
	log.WithFields(logrus.Fields{
		"event_id":   e.ID(),
		"event_type": e.Type(),
	}).Info("Function invoked")

	// Parse CloudEvent data
	var msg MessagePublishedData
	if err := e.DataAs(&msg); err != nil {
		log.WithError(err).Error("Failed to parse CloudEvent data")
		return fmt.Errorf("event.DataAs: %v", err)
	}
	log.Info("Successfully parsed CloudEvent data")

	// Log the raw message data for debugging
	log.WithField("data", string(msg.Message.Data)).Debug("Raw message data")

	// Parse the Storage notification
	var notification StorageNotification
	if err := json.Unmarshal(msg.Message.Data, &notification); err != nil {
		log.WithError(err).Error("Failed to unmarshal Storage notification")
		return fmt.Errorf("json.Unmarshal: %v", err)
	}

	logger := log.WithFields(logrus.Fields{
		"file":   notification.Name,
		"bucket": notification.Bucket,
	})
	logger.Info("Successfully parsed Storage notification")

	// Check if this is an OS image upload
	filePath := notification.Name
	logger.WithField("path", filePath).Info("Checking file path")

	if !strings.HasPrefix(filePath, "images/") {
		logger.Warn("Skipping file not in images/ directory")
		return nil // Not in the images directory
	}

	if !isOSImage(filePath) {
		logger.Warn("Skipping non-image file")
		return nil // Not an OS image
	}

	// Parse the path to extract device type and version
	// Expected format: images/{device_type}/{version}/filename.ext
	parts := strings.Split(filePath, "/")
	logger.WithField("parts", parts).Debug("Path parts")

	if len(parts) < 4 {
		logger.WithField("path", filePath).Error("Invalid file path format")
		return fmt.Errorf("invalid file path format: %s", filePath)
	}

	deviceType := parts[1]
	version := parts[2]
	logger = logger.WithFields(logrus.Fields{
		"device_type": deviceType,
		"version":     version,
	})
	logger.Info("Detected device type and version")

	// Create a storage client
	logger.Info("Creating storage client")
	client, err := storage.NewClient(ctx)
	if err != nil {
		logger.WithError(err).Error("Failed to create storage client")
		return fmt.Errorf("storage.NewClient: %v", err)
	}
	defer client.Close()

	// Get file metadata
	logger.Info("Getting file metadata")
	bucket := client.Bucket(notification.Bucket)
	obj := bucket.Object(filePath)
	attrs, err := obj.Attrs(ctx)
	if err != nil {
		logger.WithError(err).Error("Failed to get file attributes")
		return fmt.Errorf("object.Attrs: %v", err)
	}
	logger.WithField("size_bytes", attrs.Size).Info("Got file metadata")

	// Update device manifest
	logger.Info("Updating device manifest")
	if err := updateDeviceManifest(ctx, logger, bucket, deviceType, version, filePath, attrs); err != nil {
		logger.WithError(err).Error("Failed to update device manifest")
		return fmt.Errorf("updateDeviceManifest: %v", err)
	}

	// Update master manifest
	logger.Info("Updating master manifest")
	if err := updateMasterManifest(ctx, logger, bucket, deviceType, version); err != nil {
		logger.WithError(err).Error("Failed to update master manifest")
		return fmt.Errorf("updateMasterManifest: %v", err)
	}

	logger.Info("Successfully updated manifests")
	return nil
}

func updateDeviceManifest(ctx context.Context, logger *logrus.Entry, bucket *storage.BucketHandle, deviceType, version, filePath string, attrs *storage.ObjectAttrs) error {
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
		if err := json.NewDecoder(r).Decode(&manifest); err != nil {
			logger.WithError(err).Error("Failed to decode existing device manifest")
			return fmt.Errorf("json.Decode: %v", err)
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
		SizeBytes:   attrs.Size,
		IsLatest:    true,
	}

	// Write back to bucket
	logger.Info("Writing device manifest back to bucket")
	w := obj.NewWriter(ctx)
	defer w.Close()

	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(manifest); err != nil {
		logger.WithError(err).Error("Failed to write device manifest")
		return err
	}
	logger.Info("Successfully wrote device manifest")
	return nil
}

func updateMasterManifest(ctx context.Context, logger *logrus.Entry, bucket *storage.BucketHandle, deviceType, version string) error {
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
		if err := json.NewDecoder(r).Decode(&masterManifest); err != nil {
			logger.WithError(err).Error("Failed to decode existing master manifest")
			return fmt.Errorf("json.Decode: %v", err)
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

	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(masterManifest); err != nil {
		logger.WithError(err).Error("Failed to write master manifest")
		return err
	}
	logger.Info("Successfully wrote master manifest")
	return nil
}
