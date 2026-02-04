# Recovery File Support - Implementation Plan

## What We Need to Add

### 1. VersionMetadata Fields
```go
RecoveryPath      string `json:"recovery_path,omitempty"`
RecoveryChecksum  string `json:"recovery_checksum,omitempty"`
RecoverySizeBytes int64  `json:"recovery_size_bytes,omitempty"`
```

### 2. Command-Line Flag
```go
recoveryFile := flag.String("recovery-file", "", "Optional recovery/tegraflash file path")
```

### 3. Processing Logic
- Compress recovery file (if needed)
- Calculate checksum
- Upload to GCS
- Add to manifest

### 4. Verification Logic
- Verify recovery file exists in GCS
- Verify checksum matches

## Implementation Steps

1. Add fields to VersionMetadata struct
2. Add --recovery-file flag
3. Add recovery processing to main flow (parallel with OS/OTA)
4. Update manifest functions to handle recovery fields
5. Add verification for recovery files
6. Update tests

Estimated time: 30 minutes
