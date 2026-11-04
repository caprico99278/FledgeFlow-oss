//! End-to-end tests of the `ffimg` binary and its JSON/exit-code contract.
//! All images are synthesized at test time.

mod common;

use std::ffi::OsStr;
use std::path::{Path, PathBuf};

use common::*;
use image::ImageFormat;
use serde_json::{Value, json};

fn sha256_file(p: &Path) -> String {
    ffimg::sha256_hex(&std::fs::read(p).unwrap())
}

fn inspect(p: &Path, extra: &[&str]) -> (i32, Value) {
    let mut args: Vec<&OsStr> = vec![OsStr::new("inspect"), p.as_os_str()];
    args.extend(extra.iter().map(OsStr::new));
    ffimg(args)
}

fn derive(src: &Path, out: &Path, extra: &[&str]) -> (i32, Value) {
    let mut args: Vec<&OsStr> = vec![
        OsStr::new("derive"),
        src.as_os_str(),
        OsStr::new("--out"),
        out.as_os_str(),
    ];
    args.extend(extra.iter().map(OsStr::new));
    ffimg(args)
}

fn dims(v: &Value) -> (u64, u64) {
    (v["width"].as_u64().unwrap(), v["height"].as_u64().unwrap())
}

#[test]
fn version_json() {
    let (code, v) = ffimg(["version"]);
    assert_eq!(code, 0);
    assert_eq!(v, json!({"name": "ffimg", "version": "0.1.0"}));
}

#[test]
fn inspect_valid_each_format() {
    let s = Scratch::new("valid");
    let img = gradient(64, 48);
    for (fmt, name, expect) in [
        (ImageFormat::Jpeg, "a.jpg", "jpeg"),
        (ImageFormat::Png, "a.png", "png"),
        (ImageFormat::WebP, "a.webp", "webp"),
    ] {
        let p = s.write(name, &encode_rgb(&img, fmt));
        let (code, v) = inspect(&p, &[]);
        assert_eq!(code, 0, "{v}");
        assert_eq!(v["ok"], true);
        assert_eq!(v["format"], expect);
        assert_eq!(dims(&v), (64, 48));
        assert_eq!(v["bytes"], std::fs::metadata(&p).unwrap().len());
        assert_eq!(v["sha256"], sha256_file(&p));
        assert_eq!(v["has_metadata"], false);
        assert_eq!(v["errors"], json!([]));
    }
}

#[test]
fn format_is_detected_from_magic_bytes_not_extension() {
    let s = Scratch::new("magic");
    let img = gradient(32, 32);
    let png_as_jpg = s.write("photo.jpg", &encode_rgb(&img, ImageFormat::Png));
    let (code, v) = inspect(&png_as_jpg, &[]);
    assert_eq!(code, 0);
    assert_eq!(v["format"], "png");

    let webp_as_png = s.write("photo.png", &encode_rgb(&img, ImageFormat::WebP));
    let (code, v) = inspect(&webp_as_png, &[]);
    assert_eq!(code, 0);
    assert_eq!(v["format"], "webp");

    let jpeg_noext = s.write("noextension", &encode_rgb(&img, ImageFormat::Jpeg));
    let (code, v) = inspect(&jpeg_noext, &[]);
    assert_eq!(code, 0);
    assert_eq!(v["format"], "jpeg");

    let text_as_png = s.write("fake.png", b"this is plain text, not an image");
    let (code, v) = inspect(&text_as_png, &[]);
    assert_eq!(code, 2);
    assert_eq!(v["ok"], false);
    assert_eq!(v["format"], "unknown");
    assert_eq!(error_codes(&v), ["unsupported_format"]);
}

#[test]
fn other_image_formats_are_unsupported() {
    let s = Scratch::new("gif");
    // A GIF header: a real image format, but not one we accept.
    let p = s.write("x.gif", b"GIF89a\x01\x00\x01\x00\x00\x00\x00;");
    let (code, v) = inspect(&p, &[]);
    assert_eq!(code, 2);
    assert_eq!(error_codes(&v), ["unsupported_format"]);
}

#[test]
fn byte_limit() {
    let s = Scratch::new("bytes");
    let data = encode_rgb(&gradient(64, 64), ImageFormat::Png);
    let p = s.write("a.png", &data);
    let (code, v) = inspect(&p, &["--max-bytes", &(data.len() - 1).to_string()]);
    assert_eq!(code, 2);
    assert_eq!(error_codes(&v), ["too_large_bytes"]);
    assert_eq!(v["bytes"], data.len());
    assert_eq!(v["format"], "png");
    assert_eq!(v["sha256"], "");

    let (code, _) = inspect(&p, &["--max-bytes", &data.len().to_string()]);
    assert_eq!(code, 0);
}

#[test]
fn pixel_limit() {
    let s = Scratch::new("pixels");
    let p = s.write("a.jpg", &encode_rgb(&gradient(100, 50), ImageFormat::Jpeg));
    let (code, v) = inspect(&p, &["--max-pixels", "4999"]);
    assert_eq!(code, 2);
    assert_eq!(error_codes(&v), ["too_many_pixels"]);
    assert_eq!(dims(&v), (100, 50));
    let (code, _) = inspect(&p, &["--max-pixels", "5000"]);
    assert_eq!(code, 0);
}

#[test]
fn decompression_bomb_header_is_rejected_before_decoding() {
    // A tiny PNG whose IHDR claims 100000 x 100000 pixels.
    let s = Scratch::new("bomb");
    let mut data = encode_rgb(&gradient(8, 8), ImageFormat::Png);
    data[16..20].copy_from_slice(&100_000u32.to_be_bytes());
    data[20..24].copy_from_slice(&100_000u32.to_be_bytes());
    fix_png_crc(&mut data, 8);
    let p = s.write("bomb.png", &data);
    let (code, v) = inspect(&p, &[]);
    assert_eq!(code, 2);
    assert_eq!(error_codes(&v), ["too_many_pixels"]);
}

#[test]
fn zero_dimension() {
    let s = Scratch::new("zero");
    let mut data = encode_rgb(&gradient(8, 8), ImageFormat::Png);
    data[16..20].copy_from_slice(&0u32.to_be_bytes());
    fix_png_crc(&mut data, 8);
    let p = s.write("zero.png", &data);
    let (code, v) = inspect(&p, &[]);
    assert_eq!(code, 2);
    assert_eq!(error_codes(&v), ["zero_dimension"]);
}

#[test]
fn truncated_files_fail_to_decode() {
    let s = Scratch::new("trunc");
    let img = gradient(128, 96);
    for (fmt, name) in [
        (ImageFormat::Jpeg, "t.jpg"),
        (ImageFormat::Png, "t.png"),
        (ImageFormat::WebP, "t.webp"),
    ] {
        let data = encode_rgb(&img, fmt);
        let p = s.write(name, &data[..data.len() / 2]);
        let (code, v) = inspect(&p, &[]);
        assert_eq!(code, 2, "{name}: {v}");
        assert_eq!(error_codes(&v), ["decode_failed"], "{name}");
    }
}

#[test]
fn corrupt_and_empty_files() {
    let s = Scratch::new("corrupt");
    // Valid JPEG magic, garbage afterwards.
    let mut junk = vec![0xFF, 0xD8, 0xFF];
    junk.extend((0..2048u32).map(|i| (i.wrapping_mul(2_654_435_761) >> 11) as u8));
    let p = s.write("junk.jpg", &junk);
    let (code, v) = inspect(&p, &[]);
    assert_eq!(code, 2, "{v}");
    assert_eq!(v["format"], "jpeg");
    assert_eq!(v["ok"], false);

    let p = s.write("empty.png", b"");
    let (code, v) = inspect(&p, &[]);
    assert_eq!(code, 2);
    assert_eq!(error_codes(&v), ["unsupported_format"]);
}

#[test]
fn missing_file_is_io_error_exit_1() {
    let s = Scratch::new("missing");
    let (code, v) = inspect(&s.path("does-not-exist.png"), &[]);
    assert_eq!(code, 1);
    assert_eq!(v["ok"], false);
    assert_eq!(error_codes(&v), ["io_error"]);
}

#[test]
fn usage_error_prints_json_exit_1() {
    let (code, v) = ffimg(["derive", "in.png", "--max-edge", "0", "--out", "x.jpg"]);
    assert_eq!(code, 1);
    assert_eq!(error_codes(&v), ["usage"]);
    let (code, v) = ffimg([
        "derive",
        "in.png",
        "--max-edge",
        "10",
        "--out",
        "x.jpg",
        "--quality",
        "101",
    ]);
    assert_eq!(code, 1);
    assert_eq!(error_codes(&v), ["usage"]);
    let (code, v) = ffimg(["nonsense"]);
    assert_eq!(code, 1);
    assert_eq!(error_codes(&v), ["usage"]);
}

#[test]
fn metadata_detection_jpeg_exif() {
    let s = Scratch::new("exif");
    let jpeg = with_exif_app1(&encode_rgb(&gradient(80, 60), ImageFormat::Jpeg));
    let p = s.write("exif.jpg", &jpeg);
    let (code, v) = inspect(&p, &[]);
    assert_eq!(code, 0, "{v}");
    assert_eq!(v["has_metadata"], true);
}

#[test]
fn metadata_detection_png_text() {
    let s = Scratch::new("ptext");
    let png = with_png_text(&encode_rgb(&gradient(16, 16), ImageFormat::Png));
    let p = s.write("t.png", &png);
    let (code, v) = inspect(&p, &[]);
    assert_eq!(code, 0, "{v}");
    assert_eq!(v["has_metadata"], true);
}

#[test]
fn derive_strips_metadata() {
    let s = Scratch::new("strip");
    let src = s.write(
        "exif.jpg",
        &with_exif_app1(&encode_rgb(&gradient(200, 100), ImageFormat::Jpeg)),
    );
    let (_, before) = inspect(&src, &[]);
    assert_eq!(before["has_metadata"], true);
    for fmt in ["jpeg", "png", "webp"] {
        let out = s.path(&format!("out.{fmt}"));
        let (code, v) = derive(&src, &out, &["--max-edge", "100", "--format", fmt]);
        assert_eq!(code, 0, "{v}");
        let (code, i) = inspect(&out, &[]);
        assert_eq!(code, 0);
        assert_eq!(i["has_metadata"], false, "{fmt}");
        assert_eq!(i["format"], fmt);
    }
    // A PNG text chunk is stripped too.
    let src = s.write(
        "t.png",
        &with_png_text(&encode_rgb(&gradient(16, 16), ImageFormat::Png)),
    );
    let out = s.path("t-out.png");
    let (code, _) = derive(&src, &out, &["--max-edge", "16", "--format", "png"]);
    assert_eq!(code, 0);
    let (_, i) = inspect(&out, &[]);
    assert_eq!(i["has_metadata"], false);
}

fn derive_ok(s: &Scratch, w: u32, h: u32, max_edge: u32) -> (Value, PathBuf) {
    let src = s.write(
        &format!("src-{w}x{h}.png"),
        &encode_rgb(&gradient(w, h), ImageFormat::Png),
    );
    let out = s.path(&format!("out-{w}x{h}-{max_edge}.jpg"));
    let (code, v) = derive(&src, &out, &["--max-edge", &max_edge.to_string()]);
    assert_eq!(code, 0, "{v}");
    assert_eq!(v["ok"], true);
    assert_eq!(v["format"], "jpeg");
    assert_eq!(v["source_sha256"], sha256_file(&src));
    assert_eq!(v["sha256"], sha256_file(&out));
    assert_eq!(v["bytes"], std::fs::metadata(&out).unwrap().len());
    let (_, i) = inspect(&out, &[]);
    assert_eq!(dims(&i), dims(&v));
    (v, out)
}

#[test]
fn derive_preserves_aspect_ratio() {
    let s = Scratch::new("aspect");
    assert_eq!(dims(&derive_ok(&s, 400, 300, 100).0), (100, 75));
    assert_eq!(dims(&derive_ok(&s, 300, 400, 100).0), (75, 100));
    assert_eq!(dims(&derive_ok(&s, 333, 111, 100).0), (100, 33));
    assert_eq!(dims(&derive_ok(&s, 256, 256, 64).0), (64, 64));
}

#[test]
fn derive_never_upscales() {
    let s = Scratch::new("noup");
    assert_eq!(dims(&derive_ok(&s, 120, 80, 1000).0), (120, 80));
    assert_eq!(dims(&derive_ok(&s, 100, 50, 100).0), (100, 50));
}

#[test]
fn derive_alpha_input_to_each_format() {
    let s = Scratch::new("alpha");
    let src = s.write(
        "a.png",
        &encode_rgba(&gradient_rgba(50, 40), ImageFormat::Png),
    );
    for fmt in ["jpeg", "webp", "png"] {
        let out = s.path(&format!("a.{fmt}"));
        let (code, v) = derive(
            &src,
            &out,
            &["--max-edge", "25", "--format", fmt, "--quality", "70"],
        );
        assert_eq!(code, 0, "{fmt}: {v}");
        assert_eq!(dims(&v), (25, 20));
        assert_eq!(inspect(&out, &[]).1["format"], fmt);
    }
}

#[test]
fn derive_respects_input_limits() {
    let s = Scratch::new("dlimits");
    let src = s.write("a.png", &encode_rgb(&gradient(40, 40), ImageFormat::Png));
    let out = s.path("o.jpg");
    let (code, v) = derive(&src, &out, &["--max-edge", "10", "--max-pixels", "100"]);
    assert_eq!(code, 2);
    assert_eq!(error_codes(&v), ["too_many_pixels"]);
    assert!(!out.exists());
}

#[test]
fn derive_invalid_input_writes_nothing() {
    let s = Scratch::new("badderive");
    let src = s.write("bad.png", b"not an image at all");
    let out = s.path("out.jpg");
    let (code, v) = derive(&src, &out, &["--max-edge", "10"]);
    assert_eq!(code, 2);
    assert_eq!(v["ok"], false);
    assert_eq!(error_codes(&v), ["unsupported_format"]);
    assert_eq!(v["source_sha256"], sha256_file(&src));
    assert!(!out.exists());
}

#[test]
fn derive_overwrites_atomically_without_leftovers() {
    let s = Scratch::new("atomic");
    let src = s.write("src.png", &encode_rgb(&gradient(64, 64), ImageFormat::Png));
    let out = s.write("out.jpg", b"old content");
    let (code, v) = derive(&src, &out, &["--max-edge", "32"]);
    assert_eq!(code, 0, "{v}");
    assert_eq!(v["sha256"], sha256_file(&out));
    let mut names: Vec<String> = std::fs::read_dir(s.dir())
        .unwrap()
        .map(|e| e.unwrap().file_name().to_string_lossy().into_owned())
        .collect();
    names.sort();
    assert_eq!(names, ["out.jpg", "src.png"]);
}

#[test]
fn derive_to_missing_directory_is_io_error() {
    let s = Scratch::new("nodir");
    let src = s.write("src.png", &encode_rgb(&gradient(8, 8), ImageFormat::Png));
    let out = s.path("no/such/dir/out.jpg");
    let (code, v) = derive(&src, &out, &["--max-edge", "4"]);
    assert_eq!(code, 1);
    assert_eq!(error_codes(&v), ["io_error"]);
    assert_eq!(v["source_sha256"], sha256_file(&src));
}
