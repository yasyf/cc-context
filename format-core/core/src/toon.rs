//! TOON adapter over the `toon-format` crate, plus the number-safety guard
//! ported from toon.go.

use crate::value::{Number, Value};
use crate::{Error, ToonOpts};
use num_bigint::BigInt;
use num_rational::BigRational;
use serde::ser::{Error as _, SerializeMap, SerializeSeq};
use serde::{Serialize, Serializer};

/// 2^53: the largest integer magnitude whose neighborhood f64 represents
/// contiguously.
const MAX_SAFE_INTEGER: f64 = 9_007_199_254_740_992.0;

/// Largest effective decimal exponent (`decimal_rational`'s `ten.pow` argument)
/// the guard admits. f64's ~10^±308 range plus its ≤17 significant digits bound
/// any faithful round-trip at ±325; 400 clears that with margin. Past it a
/// lexeme has already tripped the finite/underflow gates or is a padded spelling
/// of an in-range value, so rejecting keeps `ten.pow` off the DoS-sized
/// exponents Rust parses silently where Go's strconv errors.
const MAX_DECIMAL_EXPONENT: u64 = 400;

/// Marshals the IR to TOON with the options' indent and delimiter.
/// toon-format canonicalizes every number through f64, so encode_toon first
/// rejects any number that round-trip would corrupt: decimals past float64
/// precision silently truncate and out-of-range exponents type-flip — and the
/// auto byte-net cannot catch truncation, which only shrinks output. Policy
/// beyond the Go port: integers with magnitude > 2^53 are rejected outright
/// (never approximate, never type-flip) where toon-go emitted them as quoted
/// decimal strings.
pub(crate) fn encode_toon(v: &Value, opts: &ToonOpts) -> Result<String, Error> {
    if let Some(n) = toon_lossy_number(v) {
        return Err(Error::UnsafeNumber(format!(
            "encode toon: number {n} does not survive the f64 canonicalization round-trip"
        )));
    }
    let options = toon_format::EncodeOptions::new()
        .with_spaces(opts.indent)
        .with_delimiter(opts.delimiter.into());
    toon_format::encode(&ToonSer(v), &options)
        .map_err(|e| Error::UnsupportedShape(format!("encode toon: {e}")))
}

/// Walks the IR for the first number the f64 canonicalization would corrupt.
fn toon_lossy_number(v: &Value) -> Option<&Number> {
    match v {
        Value::Object(fields) => fields.iter().find_map(|(_, val)| toon_lossy_number(val)),
        Value::Array(elems) => elems.iter().find_map(toon_lossy_number),
        Value::Number(n) if !toon_number_safe(n) => Some(n),
        _ => None,
    }
}

/// Reports whether the f64 canonicalization round-trip preserves `n`'s value:
/// either the shortest re-rendering reproduces the text verbatim ("3.14"), or
/// it denotes exactly the value the source text does ("1E2" → 100, "2.5e-3" →
/// 0.0025), checked by exact rational comparison. Rejected outright: values
/// f64 cannot parse finitely ("1e999", which Go's ParseFloat errors on and
/// Rust parses to inf), and any magnitude past 2^53 (all such f64s are
/// integer-valued; toon-go quoted these, this port never approximates or
/// type-flips).
pub(crate) fn toon_number_safe(n: &Number) -> bool {
    let s = n.as_str();
    let Ok(f) = s.parse::<f64>() else {
        return false;
    };
    if !f.is_finite() || f.abs() > MAX_SAFE_INTEGER {
        return false;
    }
    // Rust parses underflow ("1e-1000000000") to Ok(0.0) where Go's strconv errors;
    // an all-zero mantissa is exact zero (safe), a non-zero one underflowed (reject).
    if f == 0.0 {
        return mantissa_all_zero(s);
    }
    // Belt-and-braces: bound the effective exponent so ten.pow can't build a
    // DoS-sized BigInt whatever the mantissa width.
    if decimal_exponent(s).is_none_or(|e| e.unsigned_abs() > MAX_DECIMAL_EXPONENT) {
        return false;
    }
    let rendered = format!("{f}");
    if rendered == s {
        return true;
    }
    match (decimal_rational(s), decimal_rational(&rendered)) {
        (Some(src), Some(out)) => src == out,
        _ => false,
    }
}

/// Whether every significand digit of `s` is `0` (sign, point, and exponent
/// ignored) — i.e. the lexeme denotes exactly zero.
fn mantissa_all_zero(s: &str) -> bool {
    let mantissa = match s.find(['e', 'E']) {
        Some(i) => &s[..i],
        None => s,
    };
    mantissa.bytes().all(|b| !b.is_ascii_digit() || b == b'0')
}

/// The power of ten `decimal_rational` raises for `s`: its explicit exponent
/// less its fractional-digit count. `None` when the exponent overflows i64,
/// already rejected upstream by the finite/underflow gates.
fn decimal_exponent(s: &str) -> Option<i64> {
    let (mantissa_text, exp) = match s.find(['e', 'E']) {
        Some(i) => (&s[..i], s[i + 1..].parse::<i64>().ok()?),
        None => (s, 0),
    };
    let frac_len = mantissa_text
        .rfind('.')
        .map_or(0, |i| mantissa_text.len() - i - 1);
    exp.checked_sub(i64::try_from(frac_len).ok()?)
}

/// Parses a JSON number lexeme (sign, digits, optional fraction, optional
/// exponent) into an exact rational.
fn decimal_rational(s: &str) -> Option<BigRational> {
    let (mantissa_text, exp) = match s.find(['e', 'E']) {
        Some(i) => (&s[..i], s[i + 1..].parse::<i64>().ok()?),
        None => (s, 0),
    };
    let (int_text, frac_text) = match mantissa_text.find('.') {
        Some(i) => (&mantissa_text[..i], &mantissa_text[i + 1..]),
        None => (mantissa_text, ""),
    };
    let mantissa: BigInt = format!("{int_text}{frac_text}").parse().ok()?;
    let scale = exp.checked_sub(i64::try_from(frac_text.len()).ok()?)?;
    let ten = BigInt::from(10);
    Some(match scale {
        0.. => BigRational::from_integer(mantissa * ten.pow(u32::try_from(scale).ok()?)),
        _ => BigRational::new(mantissa, ten.pow(u32::try_from(-scale).ok()?)),
    })
}

/// Serialize bridge from the IR into toon-format's entry point. Guard-passing
/// numbers only: pure-integer lexemes go as i64 (they fit — the guard bounds
/// magnitude at 2^53), everything else as the value-exact f64.
struct ToonSer<'a>(&'a Value);

impl Serialize for ToonSer<'_> {
    fn serialize<S: Serializer>(&self, s: S) -> Result<S::Ok, S::Error> {
        match self.0 {
            Value::Null => s.serialize_unit(),
            Value::Bool(b) => s.serialize_bool(*b),
            Value::Number(n) => {
                if n.is_integer_lexeme() {
                    if let Ok(i) = n.as_str().parse::<i64>() {
                        return s.serialize_i64(i);
                    }
                }
                match n.as_str().parse::<f64>() {
                    Ok(f) => s.serialize_f64(f),
                    Err(_) => Err(S::Error::custom(
                        "unreachable: guard-validated number lexeme",
                    )),
                }
            }
            Value::String(t) => s.serialize_str(t),
            Value::Array(elems) => {
                let mut seq = s.serialize_seq(Some(elems.len()))?;
                for e in elems {
                    seq.serialize_element(&ToonSer(e))?;
                }
                seq.end()
            }
            Value::Object(fields) => {
                let mut map = s.serialize_map(Some(fields.len()))?;
                for (k, v) in fields {
                    map.serialize_entry(k, &ToonSer(v))?;
                }
                map.end()
            }
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::value::Number;

    fn safe(lexeme: &str) -> bool {
        toon_number_safe(&Number::from_lexeme(lexeme.to_string()))
    }

    #[test]
    fn integer_boundaries_around_2_53() {
        assert!(safe("9007199254740991")); // 2^53-1
        assert!(safe("9007199254740992")); // 2^53
        assert!(!safe("9007199254740993")); // 2^53+1: f64 rounds it away
        assert!(!safe("9007199254740994")); // 2^53+2: representable, still past policy bound
        assert!(!safe("18014398509481984")); // 2^54: exactly representable, rejected by policy
        assert!(safe("-9007199254740992"));
        assert!(!safe("-9007199254740993"));
    }

    #[test]
    fn i64_and_u64_boundaries_rejected() {
        for lexeme in [
            "9223372036854775806",
            "9223372036854775807",
            "9223372036854775808",
            "-9223372036854775807",
            "-9223372036854775808",
            "-9223372036854775809",
            "18446744073709551615",
            "18446744073709551616",
        ] {
            assert!(!safe(lexeme), "lexeme {lexeme}");
        }
    }

    #[test]
    fn wide_literals_rejected() {
        assert!(!safe("1234567890123456789012345")); // 25-digit int
        assert!(!safe("3.1415926535897932384626433")); // 26-digit decimal
        assert!(!safe("1e999"));
        assert!(!safe("-1e999"));
        assert!(!safe("1e308")); // integer-valued, magnitude past 2^53
    }

    #[test]
    fn value_preserving_spellings_accepted() {
        assert!(safe("-0"));
        assert!(safe("0"));
        assert!(safe("3.14"));
        assert!(safe("1.50")); // trailing zero: re-renders "1.5", value-equal
        assert!(safe("1E2")); // re-renders "100", value-equal
        assert!(safe("100"));
        assert!(safe("2.5e-3"));
        assert!(safe("0.0025"));
    }

    #[test]
    fn excess_precision_decimal_rejected() {
        assert!(!safe("3.14159265358979323846264338"));
    }

    // Finishing at all proves the ten.pow DoS is gone: Rust parses these to
    // Ok(0.0), and the guard rejects the underflow before the rational path.
    #[test]
    fn underflow_lexemes_rejected() {
        assert!(!safe("1e-1000"));
        assert!(!safe("1e-1000000"));
        assert!(!safe("1e-1000000000"));
    }

    #[test]
    fn zero_mantissa_accepted() {
        assert!(safe("0e1000000"));
        assert!(safe("-0.000E99"));
        assert!(safe("0E-1000000000"));
    }

    #[test]
    fn zero_mantissa_encodes_to_zero() {
        let opts = crate::SelectOpts::default();
        assert_eq!(
            crate::encode_as("0e1000000", crate::Format::Toon, &opts).unwrap(),
            "0"
        );
    }

    #[test]
    fn encode_toon_surfaces_unsafe_number() {
        let opts = crate::SelectOpts::default();
        let err = crate::encode_as(r#"[{"a":1e999}]"#, crate::Format::Toon, &opts).unwrap_err();
        assert!(matches!(err, Error::UnsafeNumber(_)));
    }
}
