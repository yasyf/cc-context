//! Golden-corpus pass through the WASM envelope. Every ../corpus vector is
//! marshaled into an `fc_format`-style request, run through `format_envelope`,
//! and its response asserted — proving the envelope faithfully routes to
//! format-core. Mirrors ../../core/tests/corpus.rs, minus the toon-go drift
//! file (that artifact is owned by the core harness); toon byte mismatches are
//! collected and printed, never failed. Classify-only vectors (no expected
//! output) carry no envelope result, so they assert the candidate list directly
//! just as the core harness does.

use format_wasm::format_envelope;
use serde::Deserialize;
use serde_json::{json, Value as Json};
use std::fs;
use std::path::{Path, PathBuf};

#[derive(Deserialize)]
struct Vector {
    name: String,
    input: String,
    opts: VectorOpts,
    expected_format: Option<String>,
    expected_output: Option<String>,
    expect_error: bool,
    expect_passthrough: bool,
}

#[derive(Deserialize)]
struct VectorOpts {
    format: Option<String>,
    indent: Option<usize>,
    delimiter: Option<String>,
    #[allow(dead_code)]
    strict: Option<bool>,
}

#[derive(Deserialize, Debug)]
struct Response {
    ok: Option<OkPayload>,
    err: Option<ErrPayload>,
}

#[derive(Deserialize, Debug)]
struct OkPayload {
    format: String,
    text: String,
}

#[derive(Deserialize, Debug)]
struct ErrPayload {
    kind: String,
    #[allow(dead_code)]
    msg: String,
}

enum Outcome {
    Pass,
    Fail(String),
    Skip(String),
    Drift(String),
}

fn call(input: &str, opts: &VectorOpts) -> Response {
    let format = match opts.format.as_deref() {
        None | Some("auto") => Json::Null,
        Some(name) => Json::String(name.to_string()),
    };
    let req = json!({
        "src": input,
        "format": format,
        "indent": opts.indent,
        "delimiter": opts.delimiter,
        "allow": Json::Null,
    });
    let bytes = format_envelope(serde_json::to_string(&req).unwrap().as_bytes());
    serde_json::from_slice(&bytes).unwrap()
}

fn classify_only(v: &Vector) -> Outcome {
    let candidates = match format_core::classify_candidates(&v.input) {
        Ok(c) => c,
        Err(e) => return Outcome::Fail(format!("classify: {e}")),
    };
    let got: Vec<&str> = candidates.iter().map(|f| f.as_str()).collect();
    match &v.expected_format {
        Some(expected) if got == [expected.as_str()] => Outcome::Pass,
        Some(expected) => Outcome::Fail(format!("classify {got:?}, want [{expected:?}]")),
        None => Outcome::Pass,
    }
}

fn run_vector(v: &Vector) -> Outcome {
    let is_auto = matches!(v.opts.format.as_deref(), None | Some("auto"));
    if is_auto && v.expected_output.is_none() && !v.expect_error && !v.expect_passthrough {
        return classify_only(v);
    }
    let resp = call(&v.input, &v.opts);
    if v.expect_passthrough {
        return match &resp.err {
            Some(e) if e.kind == "not_json" => Outcome::Pass,
            _ => Outcome::Fail(format!("want err not_json (passthrough), got {resp:?}")),
        };
    }
    if v.expect_error {
        return match &resp.err {
            Some(_) => Outcome::Pass,
            None => Outcome::Fail(format!("want err, got {resp:?}")),
        };
    }
    let ok = match (resp.ok, resp.err) {
        (Some(ok), _) => ok,
        // Locked >2^53 policy: forced toon on a big integer errs where a golden
        // output is asserted; the core harness skips it identically.
        (None, Some(e)) if e.kind == "unsafe_number" && v.expected_output.is_some() => {
            return Outcome::Skip(format!("locked >2^53 policy: {}", e.msg));
        }
        (None, Some(e)) => return Outcome::Fail(format!("unexpected err {}: {}", e.kind, e.msg)),
        (None, None) => return Outcome::Fail("response had neither ok nor err".into()),
    };
    if let Some(expected) = &v.expected_format {
        if ok.format != *expected {
            return Outcome::Fail(format!("chose {}, want {expected}", ok.format));
        }
    }
    match &v.expected_output {
        Some(expected) if *expected != ok.text => match ok.format.as_str() {
            "toon" => Outcome::Drift(format!("{}: toon bytes differ", v.name)),
            _ => Outcome::Fail(format!(
                "output mismatch:\n want {expected:?}\n got  {:?}",
                ok.text
            )),
        },
        _ => Outcome::Pass,
    }
}

fn corpus_dir() -> PathBuf {
    Path::new(env!("CARGO_MANIFEST_DIR")).join("../corpus")
}

#[test]
fn corpus_through_envelope() {
    let mut files: Vec<PathBuf> = fs::read_dir(corpus_dir())
        .unwrap()
        .map(|e| e.unwrap().path())
        .filter(|p| p.extension().is_some_and(|x| x == "json"))
        .collect();
    files.sort();
    assert!(!files.is_empty(), "no corpus vectors found");

    let (mut passed, mut failed, mut skipped, mut drift) = (0usize, 0usize, 0usize, 0usize);
    for path in &files {
        let file = path.file_name().unwrap().to_string_lossy().to_string();
        let vectors: Vec<Vector> =
            serde_json::from_str(&fs::read_to_string(path).unwrap()).unwrap();
        for v in &vectors {
            match run_vector(v) {
                Outcome::Pass => passed += 1,
                Outcome::Fail(msg) => {
                    failed += 1;
                    println!("FAIL  {file} :: {} — {msg}", v.name);
                }
                Outcome::Skip(msg) => {
                    skipped += 1;
                    println!("SKIP  {file} :: {} — {msg}", v.name);
                }
                Outcome::Drift(msg) => {
                    drift += 1;
                    println!("DRIFT {file} :: {msg}");
                }
            }
        }
    }
    println!(
        "\nenvelope corpus: {passed} passed, {failed} failed, {skipped} skipped, {drift} toon-drift"
    );
    assert_eq!(
        failed, 0,
        "{failed} corpus vectors failed through the envelope"
    );
}
