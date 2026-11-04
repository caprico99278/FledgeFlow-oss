//! Writes a synthetic test image (gradient + deterministic noise).
//!
//! ```text
//! cargo run -p ffimg --example synth -- <out.jpg|out.png|out.webp> <width> <height> [--exif]
//! ```
//!
//! The output format is chosen from the extension. `--exif` (JPEG only)
//! inserts a minimal APP1 "Exif" segment so that `ffimg inspect` reports
//! `has_metadata: true`, which is handy for trying out `ffimg derive`.

use image::{ImageFormat, Rgb, RgbImage};

fn main() {
    let args: Vec<String> = std::env::args().skip(1).collect();
    if args.len() < 3 {
        eprintln!("usage: synth <out.jpg|out.png|out.webp> <width> <height> [--exif]");
        std::process::exit(64);
    }
    let out = &args[0];
    let w: u32 = args[1].parse().expect("width");
    let h: u32 = args[2].parse().expect("height");
    let exif = args.iter().any(|a| a == "--exif");

    let mut s: u32 = 0x2545_F491;
    let img = RgbImage::from_fn(w, h, |x, y| {
        s ^= s << 13;
        s ^= s >> 17;
        s ^= s << 5;
        let n = (s & 0x1F) as u8;
        Rgb([
            ((x * 255 / w) as u8).wrapping_add(n),
            ((y * 255 / h) as u8).wrapping_add(n),
            (((x + y) * 255 / (w + h)) as u8) ^ n,
        ])
    });
    let format = ImageFormat::from_path(out).expect("extension must be jpg, png or webp");
    let mut data = Vec::new();
    img.write_to(&mut std::io::Cursor::new(&mut data), format)
        .expect("encode");
    if exif && format == ImageFormat::Jpeg {
        let mut payload = b"Exif\0\0II*\0\x08\0\0\0\0\0\0\0\0\0".to_vec();
        let len = u16::try_from(payload.len() + 2).expect("small segment");
        let mut with = vec![0xFF, 0xD8, 0xFF, 0xE1];
        with.extend_from_slice(&len.to_be_bytes());
        with.append(&mut payload);
        with.extend_from_slice(&data[2..]);
        data = with;
    }
    std::fs::write(out, &data).expect("write");
    println!("wrote {out}: {w}x{h}, {} bytes", data.len());
}
