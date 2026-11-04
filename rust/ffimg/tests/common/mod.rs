//! Synthetic test images generated at test time (no third-party assets).
#![allow(dead_code)]

use std::io::Cursor;
use std::path::{Path, PathBuf};
use std::process::Command;
use std::sync::atomic::{AtomicU32, Ordering};

use image::{ImageFormat, Rgb, RgbImage, Rgba, RgbaImage};
use serde_json::Value;

/// Horizontal/vertical gradient with deterministic xorshift noise.
pub fn gradient(w: u32, h: u32) -> RgbImage {
    let mut s: u32 = 0x1234_5678;
    RgbImage::from_fn(w, h, |x, y| {
        s ^= s << 13;
        s ^= s >> 17;
        s ^= s << 5;
        let n = (s & 0x0F) as u8;
        Rgb([
            ((x * 255 / w.max(1)) as u8).wrapping_add(n),
            ((y * 255 / h.max(1)) as u8).wrapping_add(n),
            128 ^ n,
        ])
    })
}

/// Gradient with an alpha ramp.
pub fn gradient_rgba(w: u32, h: u32) -> RgbaImage {
    RgbaImage::from_fn(w, h, |x, y| {
        Rgba([
            (x * 255 / w) as u8,
            (y * 255 / h) as u8,
            64,
            (x * 255 / w) as u8,
        ])
    })
}

pub fn encode_rgb(img: &RgbImage, fmt: ImageFormat) -> Vec<u8> {
    let mut v = Vec::new();
    img.write_to(&mut Cursor::new(&mut v), fmt).unwrap();
    v
}

pub fn encode_rgba(img: &RgbaImage, fmt: ImageFormat) -> Vec<u8> {
    let mut v = Vec::new();
    img.write_to(&mut Cursor::new(&mut v), fmt).unwrap();
    v
}

/// Inserts an APP1 "Exif" segment (minimal little-endian TIFF header with an
/// empty IFD) right after SOI.
pub fn with_exif_app1(jpeg: &[u8]) -> Vec<u8> {
    assert_eq!(&jpeg[..2], &[0xFF, 0xD8]);
    let mut payload = b"Exif\0\0".to_vec();
    payload.extend_from_slice(b"II*\0\x08\0\0\0"); // TIFF header, IFD0 at 8
    payload.extend_from_slice(&[0, 0]); // 0 entries
    payload.extend_from_slice(&[0, 0, 0, 0]); // no next IFD
    let len = (payload.len() + 2) as u16;
    let mut out = vec![0xFF, 0xD8, 0xFF, 0xE1];
    out.extend_from_slice(&len.to_be_bytes());
    out.extend_from_slice(&payload);
    out.extend_from_slice(&jpeg[2..]);
    out
}

/// A unique, empty scratch directory under the system temp dir.
pub struct Scratch(PathBuf);

impl Scratch {
    pub fn new(tag: &str) -> Scratch {
        static N: AtomicU32 = AtomicU32::new(0);
        let dir = std::env::temp_dir().join(format!(
            "ffimg-test-{}-{}-{}",
            tag,
            std::process::id(),
            N.fetch_add(1, Ordering::SeqCst)
        ));
        let _ = std::fs::remove_dir_all(&dir);
        std::fs::create_dir_all(&dir).unwrap();
        Scratch(dir)
    }

    pub fn path(&self, name: &str) -> PathBuf {
        self.0.join(name)
    }

    pub fn write(&self, name: &str, data: &[u8]) -> PathBuf {
        let p = self.path(name);
        std::fs::write(&p, data).unwrap();
        p
    }

    pub fn dir(&self) -> &Path {
        &self.0
    }
}

impl Drop for Scratch {
    fn drop(&mut self) {
        let _ = std::fs::remove_dir_all(&self.0);
    }
}

/// Runs the built binary, returning (exit code, parsed stdout JSON).
pub fn ffimg<I, S>(args: I) -> (i32, Value)
where
    I: IntoIterator<Item = S>,
    S: AsRef<std::ffi::OsStr>,
{
    let out = Command::new(env!("CARGO_BIN_EXE_ffimg"))
        .args(args)
        .output()
        .expect("run ffimg");
    let stdout = String::from_utf8(out.stdout).expect("utf-8 stdout");
    let lines: Vec<&str> = stdout.lines().collect();
    assert_eq!(
        lines.len(),
        1,
        "expected exactly one JSON line, got: {stdout:?}"
    );
    let json: Value = serde_json::from_str(lines[0]).expect("valid JSON");
    (out.status.code().expect("exit code"), json)
}

pub fn error_codes(v: &Value) -> Vec<String> {
    v["errors"]
        .as_array()
        .unwrap()
        .iter()
        .map(|e| e["code"].as_str().unwrap().to_string())
        .collect()
}

/// CRC-32 (IEEE) as used by PNG chunks.
pub fn crc32(data: &[u8]) -> u32 {
    let mut crc = 0xFFFF_FFFFu32;
    for &b in data {
        crc ^= u32::from(b);
        for _ in 0..8 {
            crc = if crc & 1 != 0 {
                (crc >> 1) ^ 0xEDB8_8320
            } else {
                crc >> 1
            };
        }
    }
    !crc
}

/// Recomputes the CRC of the PNG chunk whose length field starts at `pos`.
pub fn fix_png_crc(png: &mut [u8], pos: usize) {
    let len = u32::from_be_bytes(png[pos..pos + 4].try_into().unwrap()) as usize;
    let crc = crc32(&png[pos + 4..pos + 8 + len]);
    png[pos + 8 + len..pos + 12 + len].copy_from_slice(&crc.to_be_bytes());
}

/// Inserts a valid `tEXt` chunk right after IHDR.
pub fn with_png_text(png: &[u8]) -> Vec<u8> {
    let ihdr_end = 8 + 12 + 13;
    let body = b"Comment\0synthetic test";
    let mut chunk = (body.len() as u32).to_be_bytes().to_vec();
    chunk.extend_from_slice(b"tEXt");
    chunk.extend_from_slice(body);
    let crc = crc32(&chunk[4..]);
    chunk.extend_from_slice(&crc.to_be_bytes());
    let mut out = png[..ihdr_end].to_vec();
    out.extend(chunk);
    out.extend_from_slice(&png[ihdr_end..]);
    out
}
