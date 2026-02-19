package fsm

import (
	"crypto/sha256"
	"encoding/hex"
)

// imageIDNamespace is a stable, process-wide namespace used when deriving
// deterministic image IDs from idempotency keys (currently the S3 key).
//
// The exact value is not externally visible, but must remain stable over time
// so that the same idempotency key always yields the same image_id.
const imageIDNamespace = "flyio-image-manager-v1"

// DeriveImageIDFromS3Key deterministically derives an image_id from the given
// S3 object key.
//
// This function is the single source of truth for image identity and idempotency:
//   - The idempotency key for downloads is the S3 object key (s3_key).
//   - image_id is a stable SHA256 hash of (namespace, s3_key).
//   - Repeated requests with the same s3_key will always produce the same
//     image_id, and therefore converge on the same SQLite rows and FSM runs.
//
// The returned ID is a lowercase hexadecimal string with an "img_" prefix, making it
// easily identifiable in logs and databases.
//
// # Idempotency Guarantees
//
// Because image_id is deterministic:
//   - Multiple concurrent requests for the same S3 key will derive the same image_id
//   - Database unique constraints on image_id prevent duplicate downloads
//   - FSM runs for the same image will detect existing work via CheckImageDownloaded
//   - System achieves exactly-once semantics for image processing
//
// # Example
//
//	// Same S3 key always produces same image_id
//	id1 := DeriveImageIDFromS3Key("images/alpine-3.18.tar")
//	id2 := DeriveImageIDFromS3Key("images/alpine-3.18.tar")
//	// id1 == id2 (guaranteed)
//
//	// Different S3 keys produce different image_ids
//	id3 := DeriveImageIDFromS3Key("images/ubuntu-22.04.tar")
//	// id3 != id1 (with overwhelming probability)
//
// # Parameters
//
//   - s3Key: The S3 object key (e.g., "images/alpine-3.18.tar")
//
// # Returns
//
//   - string: Deterministic image ID with "img_" prefix (e.g., "img_f3e4d5c6b7a8...")
//
// # See Also
//
//   - database.CheckImageDownloaded for idempotency checks
//   - download.FSM for usage in the Download FSM
func DeriveImageIDFromS3Key(s3Key string) string {
	h := sha256.Sum256([]byte(imageIDNamespace + ":" + s3Key))
	return "img_" + hex.EncodeToString(h[:])
}
