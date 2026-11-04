//! `ffimg` command-line entry point. See the crate README for the contract.

use std::path::PathBuf;
use std::process::ExitCode;

use clap::error::ErrorKind;
use clap::{Parser, Subcommand};
use serde::Serialize;

use ffimg::derive::DEFAULT_QUALITY;
use ffimg::{
    Code, DEFAULT_MAX_BYTES, DEFAULT_MAX_PIXELS, DeriveOptions, Issue, Limits, OutputFormat, Status,
};

#[derive(Debug, Parser)]
#[command(
    name = "ffimg",
    version,
    about = "Validate and derive JPEG/PNG/WebP images. Prints one JSON object to stdout.",
    after_help = "Exit codes: 0 = valid/success, 2 = invalid input, 1 = internal or I/O error."
)]
struct Cli {
    #[command(subcommand)]
    command: Command,
}

#[derive(Debug, clap::Args)]
struct LimitArgs {
    /// Reject inputs larger than this many bytes.
    #[arg(long, default_value_t = DEFAULT_MAX_BYTES, value_parser = clap::value_parser!(u64).range(1..))]
    max_bytes: u64,
    /// Reject inputs whose width x height exceeds this.
    #[arg(long, default_value_t = DEFAULT_MAX_PIXELS, value_parser = clap::value_parser!(u64).range(1..))]
    max_pixels: u64,
}

impl LimitArgs {
    fn limits(&self) -> Limits {
        Limits {
            max_bytes: self.max_bytes,
            max_pixels: self.max_pixels,
        }
    }
}

#[derive(Debug, Subcommand)]
enum Command {
    /// Validate an image and report format, size, hash and metadata presence.
    Inspect {
        /// Input file. The format is detected from magic bytes, not the extension.
        path: PathBuf,
        #[command(flatten)]
        limits: LimitArgs,
    },
    /// Validate, resize (never upscale), strip metadata, and write atomically.
    Derive {
        /// Input file.
        input: PathBuf,
        /// Output file (written via temp file + rename).
        #[arg(long)]
        out: PathBuf,
        /// Maximum length of the longest edge of the output, in pixels.
        #[arg(long, value_parser = clap::value_parser!(u32).range(1..=65535))]
        max_edge: u32,
        /// Output format. WebP output is lossless only.
        #[arg(long, value_enum, default_value_t = OutputFormat::Jpeg)]
        format: OutputFormat,
        /// JPEG quality (ignored for png/webp).
        #[arg(long, default_value_t = DEFAULT_QUALITY, value_parser = clap::value_parser!(u8).range(1..=100))]
        quality: u8,
        #[command(flatten)]
        limits: LimitArgs,
    },
    /// Print name and version as JSON.
    Version,
}

#[derive(Serialize)]
struct VersionReport {
    name: &'static str,
    version: &'static str,
}

#[derive(Serialize)]
struct ErrorReport {
    ok: bool,
    errors: Vec<Issue>,
}

fn emit<T: Serialize>(value: &T) -> bool {
    match serde_json::to_string(value) {
        Ok(s) => {
            println!("{s}");
            true
        }
        Err(e) => {
            eprintln!("ffimg: cannot serialize output: {e}");
            false
        }
    }
}

fn fail(code: Code, message: String) -> ExitCode {
    emit(&ErrorReport {
        ok: false,
        errors: vec![Issue::new(code, message)],
    });
    ExitCode::from(Status::Internal.exit_code() as u8)
}

fn finish<T: Serialize>(value: &T, status: Status) -> ExitCode {
    if emit(value) {
        ExitCode::from(status.exit_code() as u8)
    } else {
        ExitCode::from(Status::Internal.exit_code() as u8)
    }
}

fn run(cli: Cli) -> ExitCode {
    match cli.command {
        Command::Inspect { path, limits } => {
            let report = ffimg::inspect_file(&path, &limits.limits());
            finish(&report, report.status())
        }
        Command::Derive {
            input,
            out,
            max_edge,
            format,
            quality,
            limits,
        } => {
            let opts = DeriveOptions {
                max_edge,
                format,
                quality,
                limits: limits.limits(),
            };
            let report = ffimg::derive_file(&input, &out, &opts);
            finish(&report, report.status())
        }
        Command::Version => finish(
            &VersionReport {
                name: ffimg::NAME,
                version: ffimg::VERSION,
            },
            Status::Ok,
        ),
    }
}

fn main() -> ExitCode {
    let cli = match Cli::try_parse() {
        Ok(cli) => cli,
        Err(e) if matches!(e.kind(), ErrorKind::DisplayHelp | ErrorKind::DisplayVersion) => {
            e.exit()
        }
        Err(e) => {
            // Keep stdout machine-readable: JSON there, human text on stderr.
            let _ = e.print();
            let first = e.to_string();
            let first = first.lines().next().unwrap_or("invalid usage");
            let msg = first.trim_start_matches("error: ").to_string();
            return fail(Code::Usage, msg);
        }
    };
    match std::panic::catch_unwind(std::panic::AssertUnwindSafe(|| run(cli))) {
        Ok(code) => code,
        Err(_) => fail(
            Code::Internal,
            "internal error (panic); see stderr".to_string(),
        ),
    }
}
