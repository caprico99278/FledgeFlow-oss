//! `ffimg` — pure-Rust image validation and derivation.
//!
//! The crate backs the `ffimg` command-line tool, whose JSON output is a
//! stable contract intended to be called from other languages:
//!
//! * [`inspect`](crate::inspect) validates an input (format from magic bytes,
//!   byte/pixel limits, full decode under allocation limits, metadata
//!   detection).
//! * [`derive`](crate::derive) re-encodes a validated input into a resized,
//!   metadata-free JPEG/PNG/WebP and writes it atomically.
//!
//! ```
//! use ffimg::{derive_bytes, inspect_bytes, DeriveOptions, Limits};
//!
//! // Build a tiny synthetic PNG in memory.
//! let img = image::RgbImage::from_fn(64, 32, |x, y| image::Rgb([x as u8 * 4, y as u8 * 8, 128]));
//! let mut png = Vec::new();
//! img.write_to(&mut std::io::Cursor::new(&mut png), image::ImageFormat::Png).unwrap();
//!
//! let report = inspect_bytes(&png, &Limits::default());
//! assert!(report.ok);
//! assert_eq!((report.width, report.height), (64, 32));
//!
//! let (derived, bytes) = derive_bytes(&png, &DeriveOptions::new(16));
//! assert!(derived.ok);
//! assert_eq!((derived.width, derived.height), (16, 8));
//! assert!(bytes.is_some());
//! ```

#![forbid(unsafe_code)]
#![warn(missing_docs)]

pub mod derive;
pub mod detect;
pub mod error;
pub mod inspect;

pub use derive::{DeriveOptions, DeriveReport, OutputFormat, derive_bytes, derive_file};
pub use detect::Format;
pub use error::{Code, Issue, Status};
pub use inspect::{
    DEFAULT_MAX_BYTES, DEFAULT_MAX_PIXELS, InspectReport, Limits, inspect_bytes, inspect_file,
};

use sha2::{Digest, Sha256};

/// Crate name, reported by `ffimg version`.
pub const NAME: &str = env!("CARGO_PKG_NAME");
/// Crate version, reported by `ffimg version`.
pub const VERSION: &str = env!("CARGO_PKG_VERSION");

/// Lowercase hex SHA-256 of `data`.
pub fn sha256_hex(data: &[u8]) -> String {
    hex::encode(Sha256::digest(data))
}
