//! Format detection from magic bytes and lightweight, allocation-free header
//! scanning (dimensions and metadata presence).
//!
//! Everything in this module works on a byte slice, never trusts length
//! fields without bounds checks, and never allocates proportionally to the
//! input. It runs *before* the real decoder so that obviously hostile inputs
//! (zero or gigantic dimensions) are rejected without decoding.

use serde::Serialize;

/// Image container format, detected from magic bytes (never from the file
/// extension).
#[derive(Debug, Clone, Copy, PartialEq, Eq, Hash, Serialize)]
#[serde(rename_all = "lowercase")]
pub enum Format {
    /// JPEG / JFIF / EXIF JPEG.
    Jpeg,
    /// PNG.
    Png,
    /// WebP (RIFF container).
    Webp,
    /// Anything else.
    Unknown,
}

impl Format {
    /// Lowercase string form used in JSON output.
    pub const fn as_str(self) -> &'static str {
        match self {
            Format::Jpeg => "jpeg",
            Format::Png => "png",
            Format::Webp => "webp",
            Format::Unknown => "unknown",
        }
    }

    pub(crate) fn to_image_format(self) -> Option<image::ImageFormat> {
        match self {
            Format::Jpeg => Some(image::ImageFormat::Jpeg),
            Format::Png => Some(image::ImageFormat::Png),
            Format::Webp => Some(image::ImageFormat::WebP),
            Format::Unknown => None,
        }
    }
}

impl std::fmt::Display for Format {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.write_str(self.as_str())
    }
}

const PNG_SIG: &[u8; 8] = b"\x89PNG\r\n\x1a\n";

/// Detects the format from the first bytes of the data.
///
/// ```
/// use ffimg::detect::{detect_format, Format};
/// assert_eq!(detect_format(b"\xFF\xD8\xFF\xE0rest"), Format::Jpeg);
/// assert_eq!(detect_format(b"GIF89a"), Format::Unknown);
/// ```
pub fn detect_format(data: &[u8]) -> Format {
    if data.len() >= 3 && data[0] == 0xFF && data[1] == 0xD8 && data[2] == 0xFF {
        Format::Jpeg
    } else if data.len() >= 8 && &data[..8] == PNG_SIG {
        Format::Png
    } else if data.len() >= 12 && &data[..4] == b"RIFF" && &data[8..12] == b"WEBP" {
        Format::Webp
    } else {
        Format::Unknown
    }
}

/// Result of a header scan.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Default)]
pub struct Scan {
    /// Dimensions declared by the header, if they could be found.
    pub dimensions: Option<(u32, u32)>,
    /// Best-effort: `true` if an EXIF, XMP, ICC, IPTC or textual metadata
    /// block was found.
    pub has_metadata: bool,
    /// JPEG only: the entropy-coded data after the first SOS is not
    /// terminated by an EOI marker, i.e. the file was cut off. Some JPEG
    /// decoders silently pad truncated scans with grey, so this structural
    /// check is needed to reject truncated uploads.
    pub truncated: bool,
}

/// Scans the headers of `data` (which must be of `format`).
pub fn scan(data: &[u8], format: Format) -> Scan {
    match format {
        Format::Jpeg => scan_jpeg(data),
        Format::Png => scan_png(data),
        Format::Webp => scan_webp(data),
        Format::Unknown => Scan::default(),
    }
}

fn be16(d: &[u8], at: usize) -> Option<u16> {
    d.get(at..at + 2).map(|b| u16::from_be_bytes([b[0], b[1]]))
}

fn be32(d: &[u8], at: usize) -> Option<u32> {
    d.get(at..at + 4)
        .map(|b| u32::from_be_bytes([b[0], b[1], b[2], b[3]]))
}

fn le16(d: &[u8], at: usize) -> Option<u16> {
    d.get(at..at + 2).map(|b| u16::from_le_bytes([b[0], b[1]]))
}

fn le24(d: &[u8], at: usize) -> Option<u32> {
    d.get(at..at + 3)
        .map(|b| u32::from_le_bytes([b[0], b[1], b[2], 0]))
}

fn le32(d: &[u8], at: usize) -> Option<u32> {
    d.get(at..at + 4)
        .map(|b| u32::from_le_bytes([b[0], b[1], b[2], b[3]]))
}

/// Walks JPEG marker segments up to the first SOS.
fn scan_jpeg(d: &[u8]) -> Scan {
    let mut out = Scan::default();
    let mut pos = 2; // after SOI
    loop {
        // Find the next marker (0xFF followed by a non-0xFF byte).
        match d.get(pos) {
            Some(0xFF) => {}
            // Data ended before the first scan.
            None => {
                out.truncated = true;
                return out;
            }
            // Not a marker: malformed, leave the verdict to the decoder.
            Some(_) => return out,
        }
        while d.get(pos) == Some(&0xFF) {
            pos += 1;
        }
        let Some(&marker) = d.get(pos) else {
            out.truncated = true;
            return out;
        };
        pos += 1;
        // Standalone markers without a length field.
        if marker == 0x01 || (0xD0..=0xD7).contains(&marker) {
            continue;
        }
        if marker == 0xD9 {
            // EOI before any scan: no image data at all.
            out.truncated = true;
            return out;
        }
        if marker == 0xDA {
            // Start of scan: no more header segments. Inside entropy-coded
            // data 0xFF is always followed by 0x00 (stuffing) or RSTn, so
            // the byte pair FF D9 can only be a real EOI marker.
            out.truncated = !d[pos..].windows(2).any(|w| w == [0xFF, 0xD9]);
            return out;
        }
        let Some(len) = be16(d, pos) else {
            out.truncated = true;
            return out;
        };
        let len = len as usize;
        if len < 2 {
            return out;
        }
        let body = d.get(pos + 2..(pos + len).min(d.len())).unwrap_or(&[]);
        match marker {
            // SOF0..SOF15 except DHT (C4), JPG (C8), DAC (CC).
            0xC0..=0xCF if marker != 0xC4 && marker != 0xC8 && marker != 0xCC => {
                if out.dimensions.is_none()
                    && let (Some(h), Some(w)) = (be16(body, 1), be16(body, 3))
                {
                    out.dimensions = Some((u32::from(w), u32::from(h)));
                }
            }
            // APP1: EXIF or XMP. APP2: ICC profile (or MPF/FlashPix).
            // APP13: Photoshop/IPTC. COM: free-form comment.
            0xE1 | 0xE2 | 0xED | 0xFE => out.has_metadata = true,
            _ => {}
        }
        pos += len;
    }
}

/// Walks PNG chunks up to IEND.
fn scan_png(d: &[u8]) -> Scan {
    let mut out = Scan::default();
    let mut pos = 8;
    while let (Some(len), Some(kind)) = (be32(d, pos), d.get(pos + 4..pos + 8)) {
        let data_start = pos + 8;
        if kind == b"IHDR" && out.dimensions.is_none() {
            if let (Some(w), Some(h)) = (be32(d, data_start), be32(d, data_start + 4)) {
                out.dimensions = Some((w, h));
            }
        }
        match kind {
            b"eXIf" | b"iTXt" | b"tEXt" | b"zTXt" | b"iCCP" => out.has_metadata = true,
            b"IEND" => break,
            _ => {}
        }
        // len + type + data + crc; saturating to stop on absurd lengths.
        pos = match data_start
            .checked_add(len as usize)
            .and_then(|p| p.checked_add(4))
        {
            Some(p) => p,
            None => break,
        };
    }
    out
}

/// Walks the chunks of a RIFF/WebP container.
fn scan_webp(d: &[u8]) -> Scan {
    let mut out = Scan::default();
    let mut pos = 12;
    while let (Some(kind), Some(len)) = (d.get(pos..pos + 4), le32(d, pos + 4)) {
        let start = pos + 8;
        let body = d.get(start..start.saturating_add(len as usize).min(d.len()));
        let body = body.unwrap_or(&[]);
        match kind {
            b"VP8X" => {
                if let (Some(w), Some(h)) = (le24(body, 4), le24(body, 7)) {
                    out.dimensions = Some((w + 1, h + 1));
                }
                // Flags: bit 5 ICC, bit 3 EXIF, bit 2 XMP.
                if body.first().is_some_and(|f| f & 0b0010_1100 != 0) {
                    out.has_metadata = true;
                }
            }
            b"VP8 " if out.dimensions.is_none() => {
                if body.get(3..6) == Some(&[0x9D, 0x01, 0x2A]) {
                    if let (Some(w), Some(h)) = (le16(body, 6), le16(body, 8)) {
                        out.dimensions = Some((u32::from(w & 0x3FFF), u32::from(h & 0x3FFF)));
                    }
                }
            }
            b"VP8L" if out.dimensions.is_none() => {
                if body.first() == Some(&0x2F) {
                    if let Some(bits) = le32(body, 1) {
                        let w = (bits & 0x3FFF) + 1;
                        let h = ((bits >> 14) & 0x3FFF) + 1;
                        out.dimensions = Some((w, h));
                    }
                }
            }
            b"EXIF" | b"XMP " | b"ICCP" => out.has_metadata = true,
            _ => {}
        }
        // Chunks are padded to an even size.
        let padded = (len as usize).saturating_add(len as usize & 1);
        pos = match start.checked_add(padded) {
            Some(p) => p,
            None => break,
        };
    }
    out
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn detects_magic_bytes() {
        assert_eq!(detect_format(b"\xFF\xD8\xFF\xDB"), Format::Jpeg);
        assert_eq!(detect_format(b"\x89PNG\r\n\x1a\n...."), Format::Png);
        assert_eq!(detect_format(b"RIFF\0\0\0\0WEBPVP8 "), Format::Webp);
        assert_eq!(detect_format(b"RIFF\0\0\0\0WAVEfmt "), Format::Unknown);
        assert_eq!(detect_format(b"GIF89a"), Format::Unknown);
        assert_eq!(detect_format(b""), Format::Unknown);
        assert_eq!(detect_format(b"\xFF\xD8"), Format::Unknown);
    }

    fn png_chunk(kind: &[u8; 4], data: &[u8]) -> Vec<u8> {
        let mut v = (data.len() as u32).to_be_bytes().to_vec();
        v.extend_from_slice(kind);
        v.extend_from_slice(data);
        v.extend_from_slice(&[0, 0, 0, 0]); // CRC is not checked by the scanner
        v
    }

    fn png_with(extra: &[u8; 4], w: u32, h: u32) -> Vec<u8> {
        let mut ihdr = w.to_be_bytes().to_vec();
        ihdr.extend_from_slice(&h.to_be_bytes());
        ihdr.extend_from_slice(&[8, 2, 0, 0, 0]);
        let mut v = PNG_SIG.to_vec();
        v.extend(png_chunk(b"IHDR", &ihdr));
        v.extend(png_chunk(extra, b"k\0v"));
        v.extend(png_chunk(b"IEND", &[]));
        v
    }

    #[test]
    fn png_scan() {
        let s = scan(&png_with(b"tEXt", 3, 4), Format::Png);
        assert_eq!(s.dimensions, Some((3, 4)));
        assert!(s.has_metadata);
        for kind in [b"eXIf", b"iTXt", b"zTXt", b"iCCP"] {
            assert!(scan(&png_with(kind, 1, 1), Format::Png).has_metadata);
        }
        let s = scan(&png_with(b"gAMA", 0, 9), Format::Png);
        assert_eq!(s.dimensions, Some((0, 9)));
        assert!(!s.has_metadata);
    }

    #[test]
    fn jpeg_scan() {
        // SOI, APP0 (JFIF), APP1 (Exif), SOF0 (h=20, w=30), SOS
        let mut d = vec![0xFF, 0xD8];
        d.extend([0xFF, 0xE0, 0x00, 0x07, b'J', b'F', b'I', b'F', 0]);
        d.extend([0xFF, 0xE1, 0x00, 0x08, b'E', b'x', b'i', b'f', 0, 0]);
        d.extend([0xFF, 0xC0, 0x00, 0x0B, 8, 0, 20, 0, 30, 1, 1, 0x11, 0]);
        d.extend([0xFF, 0xDA, 0x00, 0x02]);
        let s = scan(&d, Format::Jpeg);
        assert_eq!(s.dimensions, Some((30, 20)));
        assert!(s.has_metadata);
        assert!(s.truncated, "no EOI after SOS");
        d.extend([0x12, 0xFF, 0x00, 0x34, 0xFF, 0xD9]);
        assert!(!scan(&d, Format::Jpeg).truncated);

        // Without APP1 there is no metadata.
        let mut d = vec![0xFF, 0xD8];
        d.extend([0xFF, 0xC2, 0x00, 0x0B, 8, 0, 0, 0, 5, 1, 1, 0x11, 0]);
        let s = scan(&d, Format::Jpeg);
        assert_eq!(s.dimensions, Some((5, 0)));
        assert!(!s.has_metadata);
    }

    #[test]
    fn webp_scan() {
        let mut d = b"RIFF\0\0\0\0WEBP".to_vec();
        // VP8L: signature 0x2F, width-1 = 9, height-1 = 4
        let bits: u32 = 9 | (4 << 14);
        d.extend(b"VP8L");
        d.extend(5u32.to_le_bytes());
        d.push(0x2F);
        d.extend(bits.to_le_bytes());
        d.push(0); // pad
        let s = scan(&d, Format::Webp);
        assert_eq!(s.dimensions, Some((10, 5)));
        assert!(!s.has_metadata);

        d.extend(b"EXIF");
        d.extend(2u32.to_le_bytes());
        d.extend([0, 0]);
        assert!(scan(&d, Format::Webp).has_metadata);
    }

    #[test]
    fn scanners_survive_garbage() {
        let junk: Vec<u8> = (0..4096u32)
            .map(|i| (i.wrapping_mul(2_654_435_761) >> 13) as u8)
            .collect();
        for f in [Format::Jpeg, Format::Png, Format::Webp, Format::Unknown] {
            let _ = scan(&junk, f);
            for n in 0..64 {
                let _ = scan(&junk[..n], f);
            }
        }
        // Absurd lengths must not panic or loop forever.
        let mut d = PNG_SIG.to_vec();
        d.extend([0xFF, 0xFF, 0xFF, 0xFF]);
        d.extend(b"IHDR");
        let _ = scan(&d, Format::Png);
        let mut d = b"RIFF\0\0\0\0WEBP".to_vec();
        d.extend(b"VP8X");
        d.extend([0xFF, 0xFF, 0xFF, 0xFF]);
        let _ = scan(&d, Format::Webp);
    }
}
