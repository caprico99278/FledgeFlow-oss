//! Machine-readable error codes and the exit-status convention.

use serde::{Serialize, Serializer};

/// Stable, machine-readable error code reported in the `errors[].code` field.
///
/// The string form (see [`Code::as_str`]) is part of the CLI contract and must
/// not change within a major version. New codes may be added in minor versions.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Hash)]
pub enum Code {
    /// Input file is larger than `--max-bytes`.
    TooLargeBytes,
    /// Width x height exceeds `--max-pixels`.
    TooManyPixels,
    /// Magic bytes do not match JPEG, PNG or WebP.
    UnsupportedFormat,
    /// The data looks like a supported format but could not be decoded
    /// (truncated, corrupt, unsupported feature of that format, ...).
    DecodeFailed,
    /// The header declares a zero width or height.
    ZeroDimension,
    /// The decoder refused to continue because a resource limit
    /// (allocation size, dimensions) would be exceeded.
    LimitsExceeded,
    /// The file could not be read or the output could not be written.
    IoError,
    /// The decoded image could not be encoded into the requested format.
    EncodeFailed,
    /// Invalid command-line usage.
    Usage,
    /// Unexpected internal failure (e.g. a caught panic).
    Internal,
}

impl Code {
    /// The stable string form used in JSON output.
    pub const fn as_str(self) -> &'static str {
        match self {
            Code::TooLargeBytes => "too_large_bytes",
            Code::TooManyPixels => "too_many_pixels",
            Code::UnsupportedFormat => "unsupported_format",
            Code::DecodeFailed => "decode_failed",
            Code::ZeroDimension => "zero_dimension",
            Code::LimitsExceeded => "limits_exceeded",
            Code::IoError => "io_error",
            Code::EncodeFailed => "encode_failed",
            Code::Usage => "usage",
            Code::Internal => "internal",
        }
    }

    /// `true` when the code describes a problem of the environment or the
    /// tool itself (exit code 1) rather than of the input image (exit code 2).
    pub const fn is_internal(self) -> bool {
        matches!(
            self,
            Code::IoError | Code::EncodeFailed | Code::Usage | Code::Internal
        )
    }
}

impl std::fmt::Display for Code {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.write_str(self.as_str())
    }
}

impl Serialize for Code {
    fn serialize<S: Serializer>(&self, s: S) -> Result<S::Ok, S::Error> {
        s.serialize_str(self.as_str())
    }
}

/// One entry of the `errors` array.
#[derive(Debug, Clone, PartialEq, Eq, Serialize)]
pub struct Issue {
    /// Machine-readable code.
    pub code: Code,
    /// Human-readable explanation. Not part of the stable contract.
    pub message: String,
}

impl Issue {
    /// Creates a new issue.
    pub fn new(code: Code, message: impl Into<String>) -> Self {
        Issue {
            code,
            message: message.into(),
        }
    }
}

/// Overall outcome of a command, mapped onto the process exit code.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Status {
    /// Input valid, operation succeeded (exit code 0).
    Ok,
    /// Internal or I/O error (exit code 1).
    Internal,
    /// The input is invalid (exit code 2).
    Invalid,
}

impl Status {
    /// Derives the status from a list of issues: any internal issue wins,
    /// otherwise any issue means invalid input, otherwise OK.
    pub fn from_issues(issues: &[Issue]) -> Status {
        if issues.iter().any(|i| i.code.is_internal()) {
            Status::Internal
        } else if issues.is_empty() {
            Status::Ok
        } else {
            Status::Invalid
        }
    }

    /// Process exit code for this status.
    pub const fn exit_code(self) -> i32 {
        match self {
            Status::Ok => 0,
            Status::Internal => 1,
            Status::Invalid => 2,
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn code_strings_are_stable() {
        assert_eq!(Code::TooLargeBytes.as_str(), "too_large_bytes");
        assert_eq!(Code::TooManyPixels.as_str(), "too_many_pixels");
        assert_eq!(Code::UnsupportedFormat.as_str(), "unsupported_format");
        assert_eq!(Code::DecodeFailed.as_str(), "decode_failed");
        assert_eq!(Code::ZeroDimension.as_str(), "zero_dimension");
    }

    #[test]
    fn status_mapping() {
        assert_eq!(Status::from_issues(&[]), Status::Ok);
        let invalid = [Issue::new(Code::DecodeFailed, "x")];
        assert_eq!(Status::from_issues(&invalid), Status::Invalid);
        let mixed = [
            Issue::new(Code::DecodeFailed, "x"),
            Issue::new(Code::IoError, "y"),
        ];
        assert_eq!(Status::from_issues(&mixed), Status::Internal);
        assert_eq!(Status::Ok.exit_code(), 0);
        assert_eq!(Status::Internal.exit_code(), 1);
        assert_eq!(Status::Invalid.exit_code(), 2);
    }

    #[test]
    fn issue_serializes_code_as_string() {
        let s = serde_json::to_string(&Issue::new(Code::ZeroDimension, "m")).unwrap();
        assert_eq!(s, r#"{"code":"zero_dimension","message":"m"}"#);
    }
}
