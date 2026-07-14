//! WASM (`wasm32-unknown-unknown`) envelope adapter over [`format_core`].
//!
//! One coarse call — [`fc_format`] — takes a JSON request envelope and returns
//! a JSON response envelope; [`fc_alloc`] hands the host a buffer to write the
//! request into. Allocations leak: a module instance is one-shot, so the host
//! instantiates, allocs, writes, formats, reads the packed result, and discards
//! the instance. The module has zero host imports (no WASI, no I/O) and, under
//! `panic = "abort"`, traps on any host-contract violation — the host treats a
//! trap as an error and never sees partial output.

#![cfg_attr(not(test), deny(clippy::unwrap_used, clippy::expect_used))]

use format_core::{
    compact_json, decode_ir, encode_as, select_encoding, Delimiter, Encoded, Error, Format,
    FormatSet, SelectOpts, ToonOpts, Value,
};

/// TOON indent used when the request omits `indent`, matching [`ToonOpts`]'s
/// default.
const DEFAULT_INDENT: usize = 2;

struct Request {
    src: String,
    format: Option<String>,
    indent: Option<usize>,
    delimiter: Option<String>,
    allow: Option<Vec<String>>,
}

/// Maps a [`format_core::Error`] variant onto its stable response `kind`, so the
/// Go host branches on policy without matching message strings.
fn error_kind(e: &Error) -> &'static str {
    match e {
        Error::NotJson(_) => "not_json",
        Error::UnknownFormat(_) => "unknown_format",
        Error::UnsupportedShape(_) => "unsupported_shape",
        Error::UnsafeNumber(_) => "unsafe_number",
    }
}

fn take(fields: &mut Vec<(String, Value)>, key: &str) -> Option<Value> {
    fields
        .iter()
        .position(|(k, _)| k == key)
        .map(|i| fields.swap_remove(i).1)
}

fn take_src(fields: &mut Vec<(String, Value)>) -> String {
    match take(fields, "src") {
        Some(Value::String(s)) => s,
        Some(other) => panic!("format-wasm: request.src must be a string, got {other:?}"),
        None => panic!("format-wasm: request.src is required"),
    }
}

fn take_opt_string(fields: &mut Vec<(String, Value)>, key: &str) -> Option<String> {
    match take(fields, key) {
        None | Some(Value::Null) => None,
        Some(Value::String(s)) => Some(s),
        Some(other) => panic!("format-wasm: request.{key} must be a string or null, got {other:?}"),
    }
}

fn take_opt_indent(fields: &mut Vec<(String, Value)>) -> Option<usize> {
    match take(fields, "indent") {
        None | Some(Value::Null) => None,
        Some(Value::Number(n)) => match n.as_str().parse::<usize>() {
            Ok(i) => Some(i),
            Err(_) => panic!(
                "format-wasm: request.indent must be a non-negative integer, got {:?}",
                n.as_str()
            ),
        },
        Some(other) => {
            panic!("format-wasm: request.indent must be a number or null, got {other:?}")
        }
    }
}

fn take_opt_allow(fields: &mut Vec<(String, Value)>) -> Option<Vec<String>> {
    match take(fields, "allow") {
        None | Some(Value::Null) => None,
        Some(Value::Array(elems)) => Some(
            elems
                .into_iter()
                .map(|e| match e {
                    Value::String(s) => s,
                    other => {
                        panic!("format-wasm: request.allow entries must be strings, got {other:?}")
                    }
                })
                .collect(),
        ),
        Some(other) => panic!("format-wasm: request.allow must be an array or null, got {other:?}"),
    }
}

fn parse_request(text: &str) -> Request {
    let mut fields = match decode_ir(text) {
        Ok(Value::Object(fields)) => fields,
        Ok(other) => panic!("format-wasm: request envelope must be a JSON object, got {other:?}"),
        Err(e) => panic!("format-wasm: request envelope is not valid JSON: {e}"),
    };
    Request {
        src: take_src(&mut fields),
        format: take_opt_string(&mut fields, "format"),
        indent: take_opt_indent(&mut fields),
        delimiter: take_opt_string(&mut fields, "delimiter"),
        allow: take_opt_allow(&mut fields),
    }
}

fn allow_set(names: &[String]) -> Result<FormatSet, Error> {
    names.iter().try_fold(FormatSet::EMPTY, |set, name| {
        Ok(set.with(name.parse::<Format>()?))
    })
}

fn run(text: &str) -> Result<Encoded, Error> {
    let req = parse_request(text);
    let opts = SelectOpts {
        allow: match &req.allow {
            Some(names) => allow_set(names)?,
            None => FormatSet::ALL,
        },
        toon: ToonOpts {
            indent: req.indent.unwrap_or(DEFAULT_INDENT),
            delimiter: match &req.delimiter {
                Some(d) => d.parse::<Delimiter>()?,
                None => Delimiter::Comma,
            },
        },
    };
    match &req.format {
        None => select_encoding(&req.src, &opts),
        Some(name) => {
            let format = name.parse::<Format>()?;
            encode_as(&req.src, format, &opts).map(|text| Encoded { format, text })
        }
    }
}

fn response_ok(format: Format, text: String) -> String {
    compact_json(&Value::Object(vec![(
        "ok".into(),
        Value::Object(vec![
            ("format".into(), Value::String(format.as_str().into())),
            ("text".into(), Value::String(text)),
        ]),
    )]))
}

fn response_err(e: &Error) -> String {
    compact_json(&Value::Object(vec![(
        "err".into(),
        Value::Object(vec![
            ("kind".into(), Value::String(error_kind(e).into())),
            ("msg".into(), Value::String(e.to_string())),
        ]),
    )]))
}

fn pack(ptr: u32, len: u32) -> u64 {
    ((ptr as u64) << 32) | len as u64
}

/// Formats a JSON request envelope into a JSON response envelope.
///
/// Rust-level entry point: [`fc_format`] wraps this across the WASM memory
/// boundary and native tests call it directly. Request:
/// `{"src", "format"?, "indent"?, "delimiter"?, "allow"?}`. Response:
/// `{"ok":{"format","text"}}` on success, `{"err":{"kind","msg"}}` on a
/// [`format_core::Error`], where `kind` mirrors the error variant. A malformed
/// envelope (invalid UTF-8, non-object, or a mistyped field) is a host-contract
/// violation and panics — under `panic = "abort"` that traps.
pub fn format_envelope(request: &[u8]) -> Vec<u8> {
    let text = match core::str::from_utf8(request) {
        Ok(text) => text,
        Err(_) => panic!("format-wasm: request bytes are not valid UTF-8"),
    };
    match run(text) {
        Ok(enc) => response_ok(enc.format, enc.text),
        Err(e) => response_err(&e),
    }
    .into_bytes()
}

/// Leaks a `len`-byte buffer and returns its address for the host to write the
/// request envelope into. One-shot: the buffer is reclaimed when the module
/// instance is dropped, never freed explicitly.
#[no_mangle]
pub extern "C" fn fc_alloc(len: u32) -> u32 {
    let mut buf = Vec::<u8>::with_capacity(len as usize);
    let ptr = buf.as_mut_ptr() as u32;
    core::mem::forget(buf);
    ptr
}

/// Formats the request envelope at `ptr`/`len` and returns the response
/// envelope's address and length packed as `(out_ptr << 32) | out_len`. The
/// output buffer leaks; the host reads it before discarding the instance.
#[no_mangle]
pub extern "C" fn fc_format(ptr: u32, len: u32) -> u64 {
    let request = unsafe { core::slice::from_raw_parts(ptr as usize as *const u8, len as usize) };
    let out = format_envelope(request);
    let packed = pack(out.as_ptr() as u32, out.len() as u32);
    core::mem::forget(out);
    packed
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::json;

    fn call(req: serde_json::Value) -> serde_json::Value {
        serde_json::from_slice(&format_envelope(
            serde_json::to_string(&req).unwrap().as_bytes(),
        ))
        .unwrap()
    }

    fn auto(src: &str) -> serde_json::Value {
        call(json!({"src": src, "format": null, "indent": null, "delimiter": null, "allow": null}))
    }

    #[test]
    fn auto_mirrors_core() {
        let src = r#"[{"a":1,"b":2},{"a":3,"b":4}]"#;
        let expected = select_encoding(src, &SelectOpts::default()).unwrap();
        let resp = auto(src);
        assert_eq!(resp["ok"]["format"], expected.format.as_str());
        assert_eq!(resp["ok"]["text"], expected.text);
    }

    #[test]
    fn forced_csv_output() {
        let resp = call(json!({
            "src": r#"[{"a":1,"b":"x"},{"a":2,"b":"y"}]"#,
            "format": "csv", "indent": null, "delimiter": null, "allow": null,
        }));
        assert_eq!(resp["ok"]["format"], "csv");
        assert_eq!(resp["ok"]["text"], "a,b\n1,x\n2,y");
    }

    #[test]
    fn forced_toon_honors_indent_and_delimiter() {
        let src = r#"[{"a":1,"b":2}]"#;
        let resp = call(json!({
            "src": src, "format": "toon", "indent": 4, "delimiter": "|", "allow": null,
        }));
        assert_eq!(resp["ok"]["format"], "toon");
        let expected = encode_as(
            src,
            Format::Toon,
            &SelectOpts {
                allow: FormatSet::ALL,
                toon: ToonOpts {
                    indent: 4,
                    delimiter: Delimiter::Pipe,
                },
            },
        )
        .unwrap();
        assert_eq!(resp["ok"]["text"], expected);
    }

    #[test]
    fn allow_narrows_auto_to_json() {
        let src = r#"[{"a":1,"b":2},{"a":3,"b":4}]"#;
        let resp = call(
            json!({"src": src, "format": null, "indent": null, "delimiter": null, "allow": ["json"]}),
        );
        assert_eq!(resp["ok"]["format"], "json");
    }

    #[test]
    fn error_kind_mapping_is_total() {
        assert_eq!(error_kind(&Error::NotJson(String::new())), "not_json");
        assert_eq!(
            error_kind(&Error::UnknownFormat(String::new())),
            "unknown_format"
        );
        assert_eq!(
            error_kind(&Error::UnsupportedShape(String::new())),
            "unsupported_shape"
        );
        assert_eq!(
            error_kind(&Error::UnsafeNumber(String::new())),
            "unsafe_number"
        );
    }

    #[test]
    fn err_not_json() {
        assert_eq!(auto("this is not json")["err"]["kind"], "not_json");
    }

    #[test]
    fn err_unknown_format() {
        let resp = call(
            json!({"src": "{\"a\":1}", "format": "yaml", "indent": null, "delimiter": null, "allow": null}),
        );
        assert_eq!(resp["err"]["kind"], "unknown_format");
    }

    #[test]
    fn err_unsupported_shape() {
        let resp = call(
            json!({"src": "{\"a\":1}", "format": "csv", "indent": null, "delimiter": null, "allow": null}),
        );
        assert_eq!(resp["err"]["kind"], "unsupported_shape");
    }

    #[test]
    fn err_unsafe_number() {
        let src = r#"{"n":123456789012345678}"#;
        assert!(
            matches!(
                encode_as(src, Format::Toon, &SelectOpts::default()),
                Err(Error::UnsafeNumber(_))
            ),
            "chosen input must exercise the >2^53 number-safety guard",
        );
        let resp = call(
            json!({"src": src, "format": "toon", "indent": null, "delimiter": null, "allow": null}),
        );
        assert_eq!(resp["err"]["kind"], "unsafe_number");
    }

    #[test]
    fn unicode_and_control_chars_survive_the_response() {
        // src mixes literal multibyte/astral chars with escaped control
        // chars (\n, \t) and quotes; expected comes from the serde_json
        // oracle, so NFC/NFD storage of the literals cannot skew it.
        let src = r#"{"s":"héllo\n\"q\"\t😀"}"#;
        let resp = call(
            json!({"src": src, "format": "json", "indent": null, "delimiter": null, "allow": null}),
        );
        let expected: serde_json::Value = serde_json::from_str(src).unwrap();
        let reparsed: serde_json::Value =
            serde_json::from_str(resp["ok"]["text"].as_str().unwrap()).unwrap();
        assert_eq!(reparsed, expected);
    }

    #[test]
    fn huge_input_mirrors_core() {
        let src = format!(
            "[{}]",
            (0..50_000)
                .map(|i| i.to_string())
                .collect::<Vec<_>>()
                .join(",")
        );
        let expected = select_encoding(&src, &SelectOpts::default()).unwrap();
        let resp = auto(&src);
        assert_eq!(resp["ok"]["format"], expected.format.as_str());
        assert_eq!(resp["ok"]["text"], expected.text);
    }

    #[test]
    fn alloc_returns_nonzero_pointer() {
        assert_ne!(fc_alloc(64), 0);
    }

    #[test]
    fn pack_splits_into_ptr_and_len() {
        let packed = pack(0xDEAD_BEEF, 0x0000_1234);
        assert_eq!((packed >> 32) as u32, 0xDEAD_BEEF);
        assert_eq!(packed as u32, 0x0000_1234);
    }
}
