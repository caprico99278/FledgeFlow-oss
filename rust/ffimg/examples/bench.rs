//! Dependency-free micro benchmark for `inspect_bytes` and `derive_bytes`.
//!
//! Run (always in release mode):
//!
//! ```text
//! cargo run --release -p ffimg --example bench            # 4000x3000, 10 iterations
//! cargo run --release -p ffimg --example bench -- 6000 4000 5
//! ```
//!
//! Inputs are generated at run time (gradient + deterministic xorshift
//! noise); no external images are used. Each case runs one untimed warm-up
//! iteration, then N timed iterations; min / median / mean wall-clock times
//! are printed. Everything is single-threaded and in-memory (no disk I/O).
//! See `docs/benchmarks.md` for recorded numbers and conditions.

use std::hint::black_box;
use std::io::Cursor;
use std::time::{Duration, Instant};

use ffimg::{DeriveOptions, Limits, OutputFormat, derive_bytes, inspect_bytes};
use image::{ImageFormat, Rgb, RgbImage};

fn synthetic(w: u32, h: u32) -> RgbImage {
    let mut s: u32 = 0x9E37_79B9;
    RgbImage::from_fn(w, h, |x, y| {
        s ^= s << 13;
        s ^= s >> 17;
        s ^= s << 5;
        let n = (s & 0x1F) as u8;
        Rgb([
            ((x * 255 / w) as u8).wrapping_add(n),
            ((y * 255 / h) as u8).wrapping_add(n),
            (((x + y) * 255 / (w + h)) as u8) ^ n,
        ])
    })
}

fn encoded(img: &RgbImage, fmt: ImageFormat) -> Vec<u8> {
    let mut v = Vec::new();
    img.write_to(&mut Cursor::new(&mut v), fmt).expect("encode");
    v
}

fn ms(d: Duration) -> f64 {
    d.as_secs_f64() * 1000.0
}

fn bench(name: &str, iters: usize, mut f: impl FnMut()) {
    f(); // warm-up
    let mut times: Vec<Duration> = (0..iters)
        .map(|_| {
            let t = Instant::now();
            f();
            t.elapsed()
        })
        .collect();
    times.sort();
    let mean = times.iter().sum::<Duration>() / iters as u32;
    println!(
        "{name:<40} min {:>9.1} ms   median {:>9.1} ms   mean {:>9.1} ms   (n={iters})",
        ms(times[0]),
        ms(times[iters / 2]),
        ms(mean)
    );
}

fn main() {
    let mut args = std::env::args()
        .skip(1)
        .map(|a| a.parse::<u32>().expect("numeric argument"));
    let w = args.next().unwrap_or(4000);
    let h = args.next().unwrap_or(3000);
    let iters = args.next().unwrap_or(10).max(1) as usize;

    let src = synthetic(w, h);
    let jpeg = encoded(&src, ImageFormat::Jpeg);
    let png = encoded(&src, ImageFormat::Png);
    println!(
        "input {w}x{h} ({:.1} MP) synthetic: jpeg {} bytes, png {} bytes",
        f64::from(w) * f64::from(h) / 1e6,
        jpeg.len(),
        png.len()
    );
    let limits = Limits::default();

    bench("inspect jpeg", iters, || {
        assert!(inspect_bytes(black_box(&jpeg), &limits).ok)
    });
    bench("inspect png", iters, || {
        assert!(inspect_bytes(black_box(&png), &limits).ok)
    });
    let to_jpeg = DeriveOptions::new(1600);
    bench("derive jpeg -> jpeg q85 max-edge 1600", iters, || {
        assert!(derive_bytes(black_box(&jpeg), &to_jpeg).0.ok)
    });
    let to_png = DeriveOptions {
        format: OutputFormat::Png,
        ..DeriveOptions::new(1600)
    };
    bench("derive png -> png max-edge 1600", iters, || {
        assert!(derive_bytes(black_box(&png), &to_png).0.ok)
    });
    let to_webp = DeriveOptions {
        format: OutputFormat::Webp,
        ..DeriveOptions::new(1600)
    };
    bench("derive jpeg -> webp(lossless) 1600", iters, || {
        assert!(derive_bytes(black_box(&jpeg), &to_webp).0.ok)
    });
}
