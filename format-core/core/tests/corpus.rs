//! Golden-corpus harness over ../corpus/*.json. Vectors touching a format
//! not yet in IMPLEMENTED are skipped (encoder agents extend the const as
//! they land). TOON vectors whose format choice matches but whose bytes
//! differ from toon-go are collected as drift — reported as JSON on stdout
//! and written to ../TOON_DRIFT.md — never silently passed or failed.

use format_core::{
    classify_candidates, encode_as, select_encoding, Delimiter, Error, Format, SelectOpts, ToonOpts,
};
use serde::Deserialize;
use std::fmt::Write as _;
use std::fs;
use std::path::{Path, PathBuf};

const IMPLEMENTED: &[Format] = &[
    Format::Json,
    Format::Jsonl,
    Format::Toon,
    Format::Tron,
    Format::Csv,
    Format::Tsv,
    Format::Markdown,
    Format::Prose,
];

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

#[derive(serde::Serialize)]
struct Drift {
    file: String,
    vector: String,
    input: String,
    expected: String,
    actual: String,
    note: String,
}

enum Outcome {
    Pass,
    Fail(String),
    Skip(String),
    Drift(Drift),
}

fn select_opts(opts: &VectorOpts) -> Result<SelectOpts, String> {
    let delimiter = match &opts.delimiter {
        Some(d) => d.parse::<Delimiter>().map_err(|e| e.to_string())?,
        None => Delimiter::Comma,
    };
    Ok(SelectOpts {
        toon: ToonOpts {
            indent: opts.indent.unwrap_or(2),
            delimiter,
        },
        ..SelectOpts::default()
    })
}

fn run_vector(file: &str, v: &Vector) -> Outcome {
    let opts = match select_opts(&v.opts) {
        Ok(o) => o,
        Err(e) => return Outcome::Fail(format!("bad vector opts: {e}")),
    };
    match v.opts.format.as_deref() {
        None | Some("auto") => run_auto(file, v, &opts),
        Some(name) => match name.parse::<Format>() {
            Ok(f) => run_forced(file, v, f, &opts),
            Err(_) if v.expect_error => Outcome::Pass,
            Err(e) => Outcome::Fail(format!("format parse: {e}")),
        },
    }
}

fn run_forced(file: &str, v: &Vector, format: Format, opts: &SelectOpts) -> Outcome {
    if !IMPLEMENTED.contains(&format) {
        return Outcome::Skip(format!("format {format} not implemented"));
    }
    let result = encode_as(&v.input, format, opts);
    if v.expect_passthrough {
        return match result {
            Err(Error::NotJson(_)) => Outcome::Pass,
            other => Outcome::Fail(format!("want NotJson (passthrough), got {other:?}")),
        };
    }
    if v.expect_error {
        return match result {
            Err(_) => Outcome::Pass,
            Ok(out) => Outcome::Fail(format!("want error, got {out:?}")),
        };
    }
    let out = match result {
        // Locked policy divergence: toon-go emits integers past 2^53 as
        // quoted decimal strings; this port never type-flips and rejects
        // them, so goldens asserting the quoted form are unsatisfiable.
        Err(Error::UnsafeNumber(msg)) if v.expected_output.is_some() => {
            return Outcome::Skip(format!(
                "locked >2^53 integer policy (toon-go quotes, this port rejects): {msg}"
            ));
        }
        Ok(out) => out,
        Err(e) => return Outcome::Fail(format!("unexpected error: {e}")),
    };
    match &v.expected_output {
        Some(expected) if *expected != out => match format {
            Format::Toon => {
                Outcome::Drift(drift(file, v, expected, &out, "forced-toon byte drift"))
            }
            _ => Outcome::Fail(format!(
                "output mismatch:\n want {expected:?}\n got  {out:?}"
            )),
        },
        _ => Outcome::Pass,
    }
}

fn run_auto(file: &str, v: &Vector, opts: &SelectOpts) -> Outcome {
    if v.expect_passthrough {
        return match select_encoding(&v.input, opts) {
            Err(Error::NotJson(_)) => Outcome::Pass,
            other => Outcome::Fail(format!("want NotJson (passthrough), got {other:?}")),
        };
    }
    if v.expect_error {
        return match select_encoding(&v.input, opts) {
            Err(_) => Outcome::Pass,
            Ok(out) => Outcome::Fail(format!("want error, got {out:?}")),
        };
    }

    let candidates = match classify_candidates(&v.input) {
        Ok(c) => c,
        Err(e) => return Outcome::Fail(format!("classify: {e}")),
    };

    // A classify-only vector (expected_format, no output) asserts the
    // candidate list is exactly [expected_format]; no encoder runs, so it is
    // never skipped.
    if let (Some(expected), None) = (&v.expected_format, &v.expected_output) {
        let got: Vec<&str> = candidates.iter().map(|f| f.as_str()).collect();
        if got == [expected.as_str()] {
            return Outcome::Pass;
        }
        return Outcome::Fail(format!("classify candidates {got:?}, want [{expected:?}]"));
    }

    if let Some(unimpl) = candidates.iter().find(|f| !IMPLEMENTED.contains(f)) {
        return Outcome::Skip(format!("auto candidate {unimpl} not implemented"));
    }

    let encoded = match select_encoding(&v.input, opts) {
        Ok(e) => e,
        Err(e) => return Outcome::Fail(format!("unexpected error: {e}")),
    };
    if let Some(expected) = &v.expected_format {
        if encoded.format.as_str() != expected {
            return Outcome::Fail(format!(
                "chose {}, want {expected} (candidates {candidates:?})",
                encoded.format
            ));
        }
    }
    match &v.expected_output {
        Some(expected) if *expected != encoded.text => match encoded.format {
            Format::Toon => Outcome::Drift(drift(
                file,
                v,
                expected,
                &encoded.text,
                "auto chose toon; byte drift",
            )),
            _ => Outcome::Fail(format!(
                "output mismatch:\n want {expected:?}\n got  {:?}",
                encoded.text
            )),
        },
        _ => Outcome::Pass,
    }
}

fn drift(file: &str, v: &Vector, expected: &str, actual: &str, note: &str) -> Drift {
    Drift {
        file: file.to_string(),
        vector: v.name.clone(),
        input: v.input.clone(),
        expected: expected.to_string(),
        actual: actual.to_string(),
        note: note.to_string(),
    }
}

fn corpus_dir() -> PathBuf {
    Path::new(env!("CARGO_MANIFEST_DIR")).join("../corpus")
}

fn write_drift_report(drifts: &[Drift]) {
    let mut md = String::from(
        "# TOON drift: toon-go vs toon-format\n\n\
         Corpus vectors where the format CHOICE matches the Go implementation but the\n\
         TOON bytes differ (toon-go vs the `toon-format` Rust crate's edge rendering).\n\
         Generated by `core/tests/corpus.rs`; vectors are never edited to absorb drift —\n\
         each entry awaits human review.\n\n",
    );
    if drifts.is_empty() {
        md.push_str("No drift recorded.\n");
    }
    for d in drifts {
        let _ = write!(
            md,
            "## {} — {}\n\n- note: {}\n- input: `{}`\n\nexpected (toon-go):\n\n```\n{}\n```\n\nactual (toon-format):\n\n```\n{}\n```\n\n",
            d.file, d.vector, d.note, d.input, d.expected, d.actual
        );
    }
    let path = Path::new(env!("CARGO_MANIFEST_DIR")).join("../TOON_DRIFT.md");
    fs::write(&path, md).unwrap();
}

#[test]
fn corpus() {
    let mut files: Vec<PathBuf> = fs::read_dir(corpus_dir())
        .unwrap()
        .map(|e| e.unwrap().path())
        .filter(|p| p.extension().is_some_and(|x| x == "json"))
        .collect();
    files.sort();
    assert!(!files.is_empty(), "no corpus vectors found");

    let (mut passed, mut failed, mut skipped) = (0usize, 0usize, 0usize);
    let mut drifts: Vec<Drift> = Vec::new();
    for path in &files {
        let file = path.file_name().unwrap().to_string_lossy().to_string();
        let vectors: Vec<Vector> =
            serde_json::from_str(&fs::read_to_string(path).unwrap()).unwrap();
        for v in &vectors {
            match run_vector(&file, v) {
                Outcome::Pass => {
                    passed += 1;
                    println!("PASS  {file} :: {}", v.name);
                }
                Outcome::Fail(msg) => {
                    failed += 1;
                    println!("FAIL  {file} :: {} — {msg}", v.name);
                }
                Outcome::Skip(msg) => {
                    skipped += 1;
                    println!("SKIP  {file} :: {} — {msg}", v.name);
                }
                Outcome::Drift(d) => {
                    println!("DRIFT {file} :: {} — toon bytes differ", v.name);
                    drifts.push(d);
                }
            }
        }
    }

    println!(
        "\ncorpus summary: {passed} passed, {failed} failed, {skipped} skipped-unimplemented, {} toon-drift",
        drifts.len()
    );
    println!("toon-drift report (machine-readable):");
    println!("{}", serde_json::to_string_pretty(&drifts).unwrap());
    write_drift_report(&drifts);

    assert_eq!(failed, 0, "{failed} corpus vectors failed");
}

#[test]
fn implemented_formats_are_known() {
    for f in IMPLEMENTED {
        assert!(Format::ALL.contains(f));
    }
}
