//! Validation of an input image (`ffimg inspect`).

use std::fs::File;
use std::io::{Cursor, Read};
use std::path::Path;

use image::{DynamicImage, ImageError, ImageReader};
use serde::Serialize;

use crate::detect::{self, Format};
use crate::error::{Code, Issue, Status};
use crate::sha256_hex;

/// Default maximum input size: 50 MiB.
pub const DEFAULT_MAX_BYTES: u64 = 52_428_800;
/// Default maximum number of pixels (width x height): 100 megapixels.
pub const DEFAULT_MAX_PIXELS: u64 = 100_000_000;

/// Upper bound on bytes per decoded pixel (16-bit RGBA) used to derive the
/// decoder allocation limit from `max_pixels`.
const MAX_BYTES_PER_PIXEL: u64 = 8;
/// Extra allocation headroom granted to decoders for internal buffers.
const ALLOC_HEADROOM: u64 = 16 * 1024 * 1024;

/// Resource limits applied to every input.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct Limits {
    /// Maximum input size in bytes.
    pub max_bytes: u64,
    /// Maximum width x height.
    pub max_pixels: u64,
}

impl Default for Limits {
    fn default() -> Self {
        Limits {
            max_bytes: DEFAULT_MAX_BYTES,
            max_pixels: DEFAULT_MAX_PIXELS,
        }
    }
}

/// JSON document printed by `ffimg inspect`.
#[derive(Debug, Clone, PartialEq, Eq, Serialize)]
pub struct InspectReport {
    /// `true` iff `errors` is empty.
    pub ok: bool,
    /// Format detected from magic bytes.
    pub format: Format,
    /// Width in pixels (0 if unknown).
    pub width: u32,
    /// Height in pixels (0 if unknown).
    pub height: u32,
    /// Size of the input in bytes.
    pub bytes: u64,
    /// Lowercase hex SHA-256 of the input. Empty when the input was not
    /// fully read (too large or I/O error).
    pub sha256: String,
    /// Best-effort detection of EXIF/XMP/ICC/IPTC/text metadata.
    pub has_metadata: bool,
    /// Problems found; empty when `ok` is `true`.
    pub errors: Vec<Issue>,
}

impl InspectReport {
    fn empty(format: Format, bytes: u64) -> Self {
        InspectReport {
            ok: false,
            format,
            width: 0,
            height: 0,
            bytes,
            sha256: String::new(),
            has_metadata: false,
            errors: Vec::new(),
        }
    }

    fn fail(mut self, issue: Issue) -> Self {
        self.ok = false;
        self.errors.push(issue);
        self
    }

    /// Outcome used for the process exit code.
    pub fn status(&self) -> Status {
        Status::from_issues(&self.errors)
    }
}

/// Validates an in-memory image and returns the report.
pub fn inspect_bytes(data: &[u8], limits: &Limits) -> InspectReport {
    validate(data, limits).0
}

/// Reads and validates the file at `path`.
///
/// The file is never read beyond `limits.max_bytes + 1` bytes.
pub fn inspect_file(path: &Path, limits: &Limits) -> InspectReport {
    match load(path, limits) {
        Loaded::Data(data) => validate(&data, limits).0,
        Loaded::Report(r) => r,
    }
}

pub(crate) enum Loaded {
    Data(Vec<u8>),
    /// Early verdict (I/O error or too large); no data available.
    Report(InspectReport),
}

/// Reads a file under the byte limit.
pub(crate) fn load(path: &Path, limits: &Limits) -> Loaded {
    let io_fail = |e: std::io::Error, what: &str| {
        Loaded::Report(InspectReport::empty(Format::Unknown, 0).fail(Issue::new(
            Code::IoError,
            format!("{what} {}: {e}", path.display()),
        )))
    };
    let file = match File::open(path) {
        Ok(f) => f,
        Err(e) => return io_fail(e, "cannot open"),
    };
    let declared = match file.metadata() {
        Ok(m) if m.is_file() => m.len(),
        Ok(_) => {
            return io_fail(
                std::io::Error::new(std::io::ErrorKind::InvalidInput, "not a regular file"),
                "cannot read",
            );
        }
        Err(e) => return io_fail(e, "cannot stat"),
    };
    let cap = limits.max_bytes.saturating_add(1);
    let mut data = Vec::with_capacity(declared.min(cap) as usize);
    if let Err(e) = file.take(cap).read_to_end(&mut data) {
        return io_fail(e, "cannot read");
    }
    if data.len() as u64 > limits.max_bytes {
        let format = detect::detect_format(&data);
        let size = declared.max(data.len() as u64);
        return Loaded::Report(InspectReport::empty(format, size).fail(too_large(size, limits)));
    }
    Loaded::Data(data)
}

fn too_large(size: u64, limits: &Limits) -> Issue {
    Issue::new(
        Code::TooLargeBytes,
        format!(
            "input is {size} bytes, exceeds max-bytes {}",
            limits.max_bytes
        ),
    )
}

fn check_dimensions(w: u32, h: u32, limits: &Limits) -> Option<Issue> {
    if w == 0 || h == 0 {
        return Some(Issue::new(
            Code::ZeroDimension,
            format!("image has zero dimension ({w}x{h})"),
        ));
    }
    let pixels = u64::from(w) * u64::from(h);
    if pixels > limits.max_pixels {
        return Some(Issue::new(
            Code::TooManyPixels,
            format!(
                "{w}x{h} = {pixels} pixels, exceeds max-pixels {}",
                limits.max_pixels
            ),
        ));
    }
    None
}

fn decoder_limits(limits: &Limits) -> image::Limits {
    let side = u32::try_from(limits.max_pixels).unwrap_or(u32::MAX);
    let mut l = image::Limits::default();
    l.max_image_width = Some(side);
    l.max_image_height = Some(side);
    l.max_alloc = Some(
        limits
            .max_pixels
            .saturating_mul(MAX_BYTES_PER_PIXEL)
            .saturating_add(ALLOC_HEADROOM),
    );
    l
}

fn decode(data: &[u8], format: image::ImageFormat, limits: &Limits) -> Result<DynamicImage, Issue> {
    let mut reader = ImageReader::with_format(Cursor::new(data), format);
    reader.limits(decoder_limits(limits));
    reader.decode().map_err(|e| match e {
        ImageError::Limits(e) => Issue::new(Code::LimitsExceeded, format!("decoder limit: {e}")),
        ImageError::IoError(e) => Issue::new(
            Code::DecodeFailed,
            format!("truncated or unreadable data: {e}"),
        ),
        e => Issue::new(Code::DecodeFailed, format!("decode failed: {e}")),
    })
}

/// Full validation. Returns the decoded image when the input is valid.
pub(crate) fn validate(data: &[u8], limits: &Limits) -> (InspectReport, Option<DynamicImage>) {
    let format = detect::detect_format(data);
    let size = data.len() as u64;
    let mut r = InspectReport::empty(format, size);
    if size > limits.max_bytes {
        return (r.fail(too_large(size, limits)), None);
    }
    r.sha256 = sha256_hex(data);

    let Some(image_format) = format.to_image_format() else {
        let issue = Issue::new(
            Code::UnsupportedFormat,
            "magic bytes do not match JPEG, PNG or WebP",
        );
        return (r.fail(issue), None);
    };

    // Cheap header pass first: reject zero / huge dimensions before decoding.
    let scan = detect::scan(data, format);
    r.has_metadata = scan.has_metadata;
    if let Some((w, h)) = scan.dimensions {
        r.width = w;
        r.height = h;
        if let Some(issue) = check_dimensions(w, h, limits) {
            return (r.fail(issue), None);
        }
    }
    if scan.truncated {
        let issue = Issue::new(
            Code::DecodeFailed,
            "truncated data: JPEG scan is not terminated by an EOI marker",
        );
        return (r.fail(issue), None);
    }

    match decode(data, image_format, limits) {
        Ok(img) => {
            r.width = img.width();
            r.height = img.height();
            if let Some(issue) = check_dimensions(r.width, r.height, limits) {
                return (r.fail(issue), None);
            }
            r.ok = true;
            (r, Some(img))
        }
        Err(issue) => (r.fail(issue), None),
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn dimension_checks() {
        let l = Limits {
            max_bytes: 10,
            max_pixels: 100,
        };
        assert_eq!(
            check_dimensions(0, 5, &l).unwrap().code,
            Code::ZeroDimension
        );
        assert_eq!(
            check_dimensions(5, 0, &l).unwrap().code,
            Code::ZeroDimension
        );
        assert_eq!(
            check_dimensions(11, 10, &l).unwrap().code,
            Code::TooManyPixels
        );
        assert!(check_dimensions(10, 10, &l).is_none());
        // No overflow on u32::MAX dimensions.
        assert_eq!(
            check_dimensions(u32::MAX, u32::MAX, &Limits::default())
                .unwrap()
                .code,
            Code::TooManyPixels
        );
    }

    #[test]
    fn unknown_format_is_unsupported() {
        let r = inspect_bytes(b"GIF89a\x01\x00\x01\x00", &Limits::default());
        assert!(!r.ok);
        assert_eq!(r.format, Format::Unknown);
        assert_eq!(r.errors[0].code, Code::UnsupportedFormat);
        assert_eq!(r.status(), Status::Invalid);
        assert_eq!(r.sha256.len(), 64);
    }

    #[test]
    fn in_memory_byte_limit() {
        let l = Limits {
            max_bytes: 4,
            max_pixels: 1,
        };
        let r = inspect_bytes(b"\xFF\xD8\xFF\xE0\x00", &l);
        assert_eq!(r.errors[0].code, Code::TooLargeBytes);
        assert_eq!(r.format, Format::Jpeg);
        assert!(r.sha256.is_empty());
    }
}
