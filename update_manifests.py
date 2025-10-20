#!/usr/bin/env python3
import argparse
import json
import os
import time
from datetime import datetime
from google.cloud import storage

def is_os_image(filename):
    """Check if a file is an OS image based on its extension."""
    _, ext = os.path.splitext(filename.lower())
    return ext in ['.img', '.zip', '.tgz']

def update_device_manifest(bucket, device_type, version, file_path, file_size):
    """Update the device-specific manifest."""
    manifest_path = f"manifests/{device_type}.json"
    print(f"Device manifest path: {manifest_path}")
    
    blob = bucket.blob(manifest_path)
    if blob.exists():
        print(f"Reading existing device manifest")
        manifest_content = blob.download_as_text()
        manifest = json.loads(manifest_content)
        print(f"Existing manifest contains {len(manifest.get('versions', {}))} versions")
    else:
        print(f"Creating new device manifest as it doesn't exist")
        manifest = {
            "device_id": device_type,
            "versions": {}
        }
    
    # Update version information
    # Set all existing versions' IsLatest to false
    for k, v in manifest["versions"].items():
        v["is_latest"] = False
        manifest["versions"][k] = v
    
    # Add or update this version and mark as latest
    print(f"Setting version {version} as latest")
    manifest["versions"][version] = {
        "release_date": datetime.now().isoformat(),
        "path": file_path,
        "size_bytes": file_size,
        "is_latest": True
    }
    
    # Write back to bucket
    print(f"Writing device manifest back to bucket")
    blob.upload_from_string(json.dumps(manifest, indent=2), content_type='application/json')
    print(f"Successfully wrote device manifest")
    return manifest

def update_master_manifest(bucket, device_type, version):
    """Update the master manifest."""
    master_manifest_path = "manifests/master.json"
    print(f"Master manifest path: {master_manifest_path}")
    
    blob = bucket.blob(master_manifest_path)
    if blob.exists():
        print(f"Reading existing master manifest")
        master_manifest_content = blob.download_as_text()
        master_manifest = json.loads(master_manifest_content)
        print(f"Existing master manifest contains {len(master_manifest.get('devices', {}))} devices")
    else:
        print(f"Creating new master manifest as it doesn't exist")
        master_manifest = {
            "devices": {}
        }
    
    # Update master manifest
    print(f"Updating master manifest for device {device_type} with version {version}")
    master_manifest["last_updated"] = datetime.now().isoformat()
    master_manifest["devices"][device_type] = {
        "latest": version,
        "manifest_path": f"manifests/{device_type}.json"
    }
    
    # Write back to bucket
    print(f"Writing master manifest back to bucket")
    blob.upload_from_string(json.dumps(master_manifest, indent=2), content_type='application/json')
    print(f"Successfully wrote master manifest")
    return master_manifest

def process_file(bucket_name, file_path):
    """Process a file in the bucket."""
    print(f"Processing file: {file_path}")
    
    # Check if this is an OS image upload
    if not file_path.startswith("images/") or not is_os_image(file_path):
        print(f"Skipping file not in images/ directory or not an OS image: {file_path}")
        return False
    
    # Parse the path to extract device type and version
    # Expected format: images/{device_type}/{version}/filename.ext
    parts = file_path.split("/")
    if len(parts) < 4:
        print(f"Invalid file path format: {file_path}")
        return False
    
    device_type = parts[1]
    version = parts[2]
    print(f"Detected device type: {device_type}, version: {version}")
    
    # Get file metadata
    storage_client = storage.Client()
    bucket = storage_client.bucket(bucket_name)
    blob = bucket.blob(file_path)
    
    if not blob.exists():
        print(f"File does not exist: {file_path}")
        return False
    
    blob.reload()  # Load up-to-date metadata
    file_size = blob.size
    print(f"File size: {file_size} bytes")
    
    # Update device manifest
    update_device_manifest(bucket, device_type, version, file_path, file_size)
    
    # Update master manifest
    update_master_manifest(bucket, device_type, version)
    
    print(f"Successfully updated manifests for {device_type} version {version}")
    return True

def list_files_in_bucket(bucket_name, prefix="images/"):
    """List all files in the bucket with the given prefix."""
    storage_client = storage.Client()
    bucket = storage_client.bucket(bucket_name)
    blobs = list(bucket.list_blobs(prefix=prefix))
    return [blob.name for blob in blobs if is_os_image(blob.name)]

def main():
    parser = argparse.ArgumentParser(description='Update OS image manifests in GCS bucket')
    parser.add_argument('--bucket', required=True, help='GCS bucket name')
    parser.add_argument('--file', help='Specific file path to process')
    parser.add_argument('--all', action='store_true', help='Process all image files in the bucket')
    parser.add_argument('--update-existing', action='store_true', 
                        help='Update manifests for existing images in the bucket')
    
    args = parser.parse_args()
    
    if args.file:
        process_file(args.bucket, args.file)
    elif args.all or args.update_existing:
        files = list_files_in_bucket(args.bucket)
        print(f"Found {len(files)} image files in bucket {args.bucket}")
        for file_path in files:
            process_file(args.bucket, file_path)
    else:
        parser.print_help()

if __name__ == "__main__":
    main() 