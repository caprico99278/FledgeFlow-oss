//! Derivation of a resized, metadata-free copy (`ffimg derive`).

use std::fs::{self, OpenOptions};
use std::io::{self, Write};
use std::path::{Path, PathBuf};
use std::time::{SystemTime, UNIX_EPOCH};

use image::codecs::jpeg::JpegEncoder;
use image::codecs::png::{CompressionType, FilterType as PngFilter, PngEncoder};
use image::codecs::webp::WebPEncoder;
use image::imageops::{self, FilterType};
use image::{
    DynamicImage, ExtendedColorType, ImageBuffer, ImageEncoder, Pixel, Rgb, RgbImage, RgbaImage,
};
use serde::Serialize;

use crate::detect::Format;
use crate::error::{Code, Issue, Status};
use crate::inspect::{self, Limits, Loaded};
use crate::sha256_hex;

/// Default JPEG quality.
pub const DEFAULT_QUALITY: u8 = 85;

/// Output encoding.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Hash, clap::ValueEnum)]
pub enum OutputFormat {
    /// Baseline JPEG, lossy, `quality` applies. Alpha is flattened onto white.
    Jpeg,
    /// PNG, lossless. `quality` is ignored.
    Png,
    /// WebP, **lossless only** (the pure-Rust encoder has no lossy mode).
    /// `quality` is ignored.
    Webp,
}

impl From<OutputFormat> for Format {
    fn from(f: OutputFormat) -> Format {
        match f {
            OutputFormat::Jpeg => Format::Jpeg,
            OutputFormat::Png => Format::Png,
            OutputFormat::Webp => Format::Webp,
        }
    }
}

/// Options for [`derive_bytes`] / [`derive_file`].
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct DeriveOptions {
    /// The longest edge of the output is at most this many pixels.
    /// Images that are already small enough are never upscaled.
    pub max_edge: u32,
    /// Output encoding.
    pub format: OutputFormat,
    /// JPEG quality 1..=100 (ignored for PNG and WebP).
    pub quality: u8,
    /// Input limits (same rules as `inspect`).
    pub limits: Limits,
}

impl DeriveOptions {
    /// Options with the given maximum edge and all other values at their
    /// defaults (JPEG, quality 85, default limits).
    pub fn new(max_edge: u32) -> Self {
        DeriveOptions {
            max_edge,
            format: OutputFormat::Jpeg,
            quality: DEFAULT_QUALITY,
            limits: Limits::default(),
        }
    }
}

/// JSON document printed by `ffimg derive`.
#[derive(Debug, Clone, PartialEq, Eq, Serialize)]
pub struct DeriveReport {
    /// `true` iff the output was written and `errors` is empty.
    pub ok: bool,
    /// Output format (the requested one).
    pub format: Format,
    /// Output width (0 on failure).
    pub width: u32,
    /// Output height (0 on failure).
    pub height: u32,
    /// Output size in bytes (0 on failure).
    pub bytes: u64,
    /// Lowercase hex SHA-256 of the output (empty on failure).
    pub sha256: String,
    /// Lowercase hex SHA-256 of the input (empty if it was not read).
    pub source_sha256: String,
    /// Problems found; empty when `ok` is `true`.
    pub errors: Vec<Issue>,
}

impl DeriveReport {
    fn failed(format: OutputFormat, source_sha256: String, errors: Vec<Issue>) -> Self {
        DeriveReport {
            ok: false,
            format: format.into(),
            width: 0,
            height: 0,
            bytes: 0,
            sha256: String::new(),
            source_sha256,
            errors,
        }
    }

    /// Outcome used for the process exit code.
    pub fn status(&self) -> Status {
        Status::from_issues(&self.errors)
    }
}

/// Computes the output size: the longest edge becomes at most `max_edge`,
/// the aspect ratio is preserved (rounded to nearest, at least 1 px), and
/// images are never upscaled.
///
/// ```
/// use ffimg::derive::target_dimensions;
/// assert_eq!(target_dimensions(4000, 3000, 1000), (1000, 750));
/// assert_eq!(target_dimensions(300, 200, 1000), (300, 200)); // no upscaling
/// ```
pub fn target_dimensions(width: u32, height: u32, max_edge: u32) -> (u32, u32) {
    let long = width.max(height);
    if long <= max_edge || max_edge == 0 {
        return (width, height);
    }
    let scale = |side: u32| -> u32 {
        let v = (u64::from(side) * u64::from(max_edge) + u64::from(long) / 2) / u64::from(long);
        v.clamp(1, u64::from(max_edge)) as u32
    };
    if width >= height {
        (max_edge, scale(height))
    } else {
        (scale(width), max_edge)
    }
}

/// Composites an RGBA image onto an opaque white background.
fn flatten_on_white(img: &RgbaImage) -> RgbImage {
    ImageBuffer::from_fn(img.width(), img.height(), |x, y| {
        let p = img.get_pixel(x, y).0;
        let a = u32::from(p[3]);
        let mix = |c: u8| ((u32::from(c) * a + 255 * (255 - a) + 127) / 255) as u8;
        Rgb([mix(p[0]), mix(p[1]), mix(p[2])])
    })
}

fn resize_to<P>(img: ImageBuffer<P, Vec<u8>>, w: u32, h: u32) -> ImageBuffer<P, Vec<u8>>
where
    P: Pixel<Subpixel = u8> + 'static,
{
    if img.width() == w && img.height() == h {
        img
    } else {
        imageops::resize(&img, w, h, FilterType::Lanczos3)
    }
}

fn encode_err(e: image::ImageError) -> Issue {
    Issue::new(Code::EncodeFailed, format!("encode failed: {e}"))
}

/// Encodes `img` (already decoded) into the requested format. Only pixel
/// data is written, so no metadata from the source can survive.
fn encode(img: &DynamicImage, opts: &DeriveOptions) -> Result<(Vec<u8>, u32, u32), Issue> {
    let (w, h) = target_dimensions(img.width(), img.height(), opts.max_edge);
    let alpha = img.color().has_alpha();
    let mut out = Vec::new();
    match opts.format {
        OutputFormat::Jpeg => {
            let rgb = if alpha {
                flatten_on_white(&img.to_rgba8())
            } else {
                img.to_rgb8()
            };
            let rgb = resize_to(rgb, w, h);
            JpegEncoder::new_with_quality(&mut out, opts.quality.clamp(1, 100))
                .encode(rgb.as_raw(), w, h, ExtendedColorType::Rgb8)
                .map_err(encode_err)?;
        }
        OutputFormat::Png | OutputFormat::Webp => {
            let (raw, color) = if alpha {
                (
                    resize_to(img.to_rgba8(), w, h).into_raw(),
                    ExtendedColorType::Rgba8,
                )
            } else {
                (
                    resize_to(img.to_rgb8(), w, h).into_raw(),
                    ExtendedColorType::Rgb8,
                )
            };
            let res = if opts.format == OutputFormat::Png {
                PngEncoder::new_with_quality(
                    &mut out,
                    CompressionType::Default,
                    PngFilter::Adaptive,
                )
                .write_image(&raw, w, h, color)
            } else {
                WebPEncoder::new_lossless(&mut out).encode(&raw, w, h, color)
            };
            res.map_err(encode_err)?;
        }
    }
    Ok((out, w, h))
}

/// Validates and derives from in-memory data. Returns the report and, on
/// success, the encoded output bytes.
pub fn derive_bytes(data: &[u8], opts: &DeriveOptions) -> (DeriveReport, Option<Vec<u8>>) {
    let (check, img) = inspect::validate(data, &opts.limits);
    let Some(img) = img else {
        return (
            DeriveReport::failed(opts.format, check.sha256, check.errors),
            None,
        );
    };
    match encode(&img, opts) {
        Ok((bytes, width, height)) => {
            let report = DeriveReport {
                ok: true,
                format: opts.format.into(),
                width,
                height,
                bytes: bytes.len() as u64,
                sha256: sha256_hex(&bytes),
                source_sha256: check.sha256,
                errors: Vec::new(),
            };
            (report, Some(bytes))
        }
        Err(issue) => (
            DeriveReport::failed(opts.format, check.sha256, vec![issue]),
            None,
        ),
    }
}

/// Reads `input`, derives, and atomically writes the result to `output`
/// (temporary file in the same directory, fsync, rename). On any failure
/// the destination is left untouched.
pub fn derive_file(input: &Path, output: &Path, opts: &DeriveOptions) -> DeriveReport {
    let data = match inspect::load(input, &opts.limits) {
        Loaded::Data(d) => d,
        Loaded::Report(r) => return DeriveReport::failed(opts.format, r.sha256, r.errors),
    };
    let (mut report, bytes) = derive_bytes(&data, opts);
    if let Some(bytes) = bytes
        && let Err(e) = write_atomic(output, &bytes)
    {
        report = DeriveReport::failed(
            opts.format,
            report.source_sha256,
            vec![Issue::new(
                Code::IoError,
                format!("cannot write {}: {e}", output.display()),
            )],
        );
    }
    report
}

fn temp_path(dir: &Path, name: &str, attempt: u32) -> PathBuf {
    let nanos = SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .map(|d| d.subsec_nanos())
        .unwrap_or(0);
    dir.join(format!(
        ".{name}.{}.{nanos}.{attempt}.ffimg-tmp",
        std::process::id()
    ))
}

/// Writes `data` to `path` atomically: the destination either keeps its old
/// content or gets the complete new content, never a partial file.
pub fn write_atomic(path: &Path, data: &[u8]) -> io::Result<()> {
    let name = path
        .file_name()
        .ok_or_else(|| io::Error::new(io::ErrorKind::InvalidInput, "output path has no file name"))?
        .to_string_lossy()
        .into_owned();
    let dir = match path.parent() {
        Some(p) if !p.as_os_str().is_empty() => p,
        _ => Path::new("."),
    };
    let mut attempt = 0;
    let (tmp, mut file) = loop {
        let tmp = temp_path(dir, &name, attempt);
        match OpenOptions::new().write(true).create_new(true).open(&tmp) {
            Ok(f) => break (tmp, f),
            Err(e) if e.kind() == io::ErrorKind::AlreadyExists && attempt < 16 => attempt += 1,
            Err(e) => return Err(e),
        }
    };
    let result = file
        .write_all(data)
        .and_then(|()| file.sync_all())
        .and_then(|()| {
            drop(file);
            fs::rename(&tmp, path)
        });
    if result.is_err() {
        let _ = fs::remove_file(&tmp);
    }
    result
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn target_dimensions_landscape_portrait_square() {
        assert_eq!(target_dimensions(4000, 3000, 1600), (1600, 1200));
        assert_eq!(target_dimensions(3000, 4000, 1600), (1200, 1600));
        assert_eq!(target_dimensions(5000, 5000, 1024), (1024, 1024));
        assert_eq!(target_dimensions(1999, 1001, 1000), (1000, 501));
    }

    #[test]
    fn target_dimensions_never_upscales() {
        assert_eq!(target_dimensions(800, 600, 1600), (800, 600));
        assert_eq!(target_dimensions(1600, 10, 1600), (1600, 10));
    }

    #[test]
    fn target_dimensions_extreme_aspect_keeps_one_pixel() {
        assert_eq!(target_dimensions(10_000, 1, 100), (100, 1));
        assert_eq!(target_dimensions(1, 10_000, 100), (1, 100));
    }

    #[test]
    fn flatten_composites_on_white() {
        let img = RgbaImage::from_raw(2, 1, vec![0, 0, 0, 0, 10, 20, 30, 255]).unwrap();
        let out = flatten_on_white(&img);
        assert_eq!(out.get_pixel(0, 0).0, [255, 255, 255]);
        assert_eq!(out.get_pixel(1, 0).0, [10, 20, 30]);
    }

    #[test]
    fn atomic_write_replaces_and_leaves_no_temp() {
        let dir = std::env::temp_dir().join(format!("ffimg-unit-{}", std::process::id()));
        fs::create_dir_all(&dir).unwrap();
        let target = dir.join("out.bin");
        write_atomic(&target, b"first").unwrap();
        write_atomic(&target, b"second").unwrap();
        assert_eq!(fs::read(&target).unwrap(), b"second");
        let leftovers: Vec<_> = fs::read_dir(&dir)
            .unwrap()
            .filter_map(Result::ok)
            .filter(|e| e.file_name().to_string_lossy().ends_with(".ffimg-tmp"))
            .collect();
        assert!(leftovers.is_empty());
        fs::remove_dir_all(&dir).unwrap();
    }
}
