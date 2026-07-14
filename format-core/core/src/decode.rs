use crate::value::{Number, Value};
use crate::Error;

/// Caps container nesting in the decoder and, transitively, every IR walker.
/// Go's encoding/json allows 10,000 (goroutine stacks grow on demand); this
/// port runs on fixed 1–2 MB native stacks and inside WASM, where recursion
/// aborts the process below that — an uncatchable SIGABRT `catch_unwind` can't
/// trap, measured at ~4,000 frames on a 1 MB stack — so the cap must leave
/// stack-safe margin. 512 gives ~8x. Because `encode_as`/`select_encoding` only
/// build IR through this decoder, the recursive walkers over it (`classify_walk`
/// in classify.rs, `tron_walk` in tron.rs, `write_compact` in json.rs) inherit
/// the same 512-deep bound and need no separate guard.
pub(crate) const MAX_NESTING_DEPTH: usize = 512;

/// Rust-only input-size sanity cap; Go's encoding/json streams without one.
pub(crate) const MAX_INPUT_BYTES: usize = 256 << 20;

/// Decodes every top-level JSON value in `src` into the ordered IR. Zero
/// values (empty or whitespace) is an error; one value is used as-is; two or
/// more (NDJSON) fold into a single array so a uniform object stream becomes
/// one table. Mirrors decode.go's decodeAll.
pub(crate) fn decode_all(src: &str) -> Result<Value, Error> {
    if src.len() > MAX_INPUT_BYTES {
        return Err(Error::NotJson(format!(
            "input of {} bytes exceeds the {MAX_INPUT_BYTES}-byte cap",
            src.len()
        )));
    }
    let mut p = Parser {
        bytes: src.as_bytes(),
        text: src,
        pos: 0,
    };
    let mut vals = Vec::new();
    loop {
        p.skip_ws();
        if p.pos >= p.bytes.len() {
            break;
        }
        vals.push(p.value(0)?);
    }
    match vals.len() {
        0 => Err(Error::NotJson("empty input".into())),
        1 => Ok(vals.remove(0)),
        _ => Ok(Value::Array(vals)),
    }
}

struct Parser<'a> {
    bytes: &'a [u8],
    text: &'a str,
    pos: usize,
}

impl Parser<'_> {
    fn err(&self, msg: &str) -> Error {
        Error::NotJson(format!("{msg} at offset {}", self.pos))
    }

    fn skip_ws(&mut self) {
        while matches!(self.bytes.get(self.pos), Some(b' ' | b'\t' | b'\n' | b'\r')) {
            self.pos += 1;
        }
    }

    fn eat(&mut self, b: u8) -> bool {
        if self.bytes.get(self.pos) == Some(&b) {
            self.pos += 1;
            return true;
        }
        false
    }

    fn value(&mut self, depth: usize) -> Result<Value, Error> {
        match self.bytes.get(self.pos) {
            Some(b'{') => self.object(depth),
            Some(b'[') => self.array(depth),
            Some(b'"') => Ok(Value::String(self.string()?)),
            Some(b't') => self.literal("true", Value::Bool(true)),
            Some(b'f') => self.literal("false", Value::Bool(false)),
            Some(b'n') => self.literal("null", Value::Null),
            Some(b'-' | b'0'..=b'9') => self.number(),
            Some(_) => Err(self.err("invalid character at start of value")),
            None => Err(self.err("unexpected end of input")),
        }
    }

    fn container_depth(&self, depth: usize) -> Result<(), Error> {
        if depth >= MAX_NESTING_DEPTH {
            return Err(self.err("exceeded max nesting depth"));
        }
        Ok(())
    }

    fn object(&mut self, depth: usize) -> Result<Value, Error> {
        self.container_depth(depth)?;
        self.pos += 1; // '{'
        let mut fields = Vec::new();
        self.skip_ws();
        if self.eat(b'}') {
            return Ok(Value::Object(fields));
        }
        loop {
            self.skip_ws();
            if self.bytes.get(self.pos) != Some(&b'"') {
                return Err(self.err("expected object key"));
            }
            let key = self.string()?;
            self.skip_ws();
            if !self.eat(b':') {
                return Err(self.err("expected ':' after object key"));
            }
            self.skip_ws();
            fields.push((key, self.value(depth + 1)?));
            self.skip_ws();
            if self.eat(b',') {
                continue;
            }
            if self.eat(b'}') {
                return Ok(Value::Object(fields));
            }
            return Err(self.err("expected ',' or '}' in object"));
        }
    }

    fn array(&mut self, depth: usize) -> Result<Value, Error> {
        self.container_depth(depth)?;
        self.pos += 1; // '['
        let mut elems = Vec::new();
        self.skip_ws();
        if self.eat(b']') {
            return Ok(Value::Array(elems));
        }
        loop {
            self.skip_ws();
            elems.push(self.value(depth + 1)?);
            self.skip_ws();
            if self.eat(b',') {
                continue;
            }
            if self.eat(b']') {
                return Ok(Value::Array(elems));
            }
            return Err(self.err("expected ',' or ']' in array"));
        }
    }

    fn literal(&mut self, lit: &str, v: Value) -> Result<Value, Error> {
        if self.text.as_bytes()[self.pos..].starts_with(lit.as_bytes()) {
            self.pos += lit.len();
            return Ok(v);
        }
        Err(self.err("invalid literal"))
    }

    /// Parses a string, byte-wise. Slices of `text` are taken only at ASCII
    /// boundaries (quote/backslash), so they always fall on char boundaries.
    fn string(&mut self) -> Result<String, Error> {
        self.pos += 1; // opening '"'
        let mut out = String::new();
        let mut seg = self.pos;
        loop {
            match self.bytes.get(self.pos) {
                None => return Err(self.err("unterminated string")),
                Some(b'"') => {
                    out.push_str(&self.text[seg..self.pos]);
                    self.pos += 1;
                    return Ok(out);
                }
                Some(b'\\') => {
                    out.push_str(&self.text[seg..self.pos]);
                    self.pos += 1;
                    self.escape(&mut out)?;
                    seg = self.pos;
                }
                Some(&c) if c < 0x20 => {
                    return Err(self.err("control character in string"));
                }
                Some(_) => self.pos += 1,
            }
        }
    }

    /// Decodes one escape after the backslash. Surrogate handling mirrors Go
    /// encoding/json's unquote: a valid `\uD800-\uDBFF`+`\uDC00-\uDFFF` pair
    /// combines; any invalid surrogate becomes U+FFFD without consuming what
    /// follows.
    fn escape(&mut self, out: &mut String) -> Result<(), Error> {
        let Some(&c) = self.bytes.get(self.pos) else {
            return Err(self.err("unterminated escape"));
        };
        self.pos += 1;
        match c {
            b'"' => out.push('"'),
            b'\\' => out.push('\\'),
            b'/' => out.push('/'),
            b'b' => out.push('\u{0008}'),
            b'f' => out.push('\u{000C}'),
            b'n' => out.push('\n'),
            b'r' => out.push('\r'),
            b't' => out.push('\t'),
            b'u' => {
                let hi = self.hex4()?;
                match hi {
                    0xD800..=0xDBFF => match self.peek_low_surrogate() {
                        Some(lo) => {
                            self.pos += 6; // consume "\uXXXX"
                            let cp = 0x10000 + ((hi - 0xD800) << 10) + (lo - 0xDC00);
                            out.push(char::from_u32(cp).unwrap_or('\u{FFFD}'));
                        }
                        None => out.push('\u{FFFD}'),
                    },
                    0xDC00..=0xDFFF => out.push('\u{FFFD}'),
                    _ => out.push(char::from_u32(hi).unwrap_or('\u{FFFD}')),
                }
            }
            _ => return Err(self.err("invalid escape character")),
        }
        Ok(())
    }

    fn hex4(&mut self) -> Result<u32, Error> {
        let mut r = 0u32;
        for _ in 0..4 {
            let Some(d) = self
                .bytes
                .get(self.pos)
                .and_then(|&b| (b as char).to_digit(16))
            else {
                return Err(self.err("invalid \\u escape"));
            };
            r = r << 4 | d;
            self.pos += 1;
        }
        Ok(r)
    }

    /// Peeks a `\uDC00-\uDFFF` low surrogate at the cursor without consuming.
    fn peek_low_surrogate(&self) -> Option<u32> {
        let b = self.bytes.get(self.pos..self.pos + 6)?;
        if b[0] != b'\\' || b[1] != b'u' {
            return None;
        }
        let mut r = 0u32;
        for &d in &b[2..6] {
            r = r << 4 | (d as char).to_digit(16)?;
        }
        (0xDC00..=0xDFFF).contains(&r).then_some(r)
    }

    /// Validates the RFC 8259 number grammar and captures the raw lexeme.
    fn number(&mut self) -> Result<Value, Error> {
        let start = self.pos;
        self.eat(b'-');
        match self.bytes.get(self.pos) {
            Some(b'0') => self.pos += 1,
            Some(b'1'..=b'9') => {
                self.pos += 1;
                self.digits();
            }
            _ => return Err(self.err("invalid number")),
        }
        if self.eat(b'.') {
            self.digits1()?;
        }
        if matches!(self.bytes.get(self.pos), Some(b'e' | b'E')) {
            self.pos += 1;
            if matches!(self.bytes.get(self.pos), Some(b'+' | b'-')) {
                self.pos += 1;
            }
            self.digits1()?;
        }
        Ok(Value::Number(Number::from_lexeme(
            self.text[start..self.pos].to_string(),
        )))
    }

    fn digits(&mut self) {
        while matches!(self.bytes.get(self.pos), Some(b'0'..=b'9')) {
            self.pos += 1;
        }
    }

    fn digits1(&mut self) -> Result<(), Error> {
        if !matches!(self.bytes.get(self.pos), Some(b'0'..=b'9')) {
            return Err(self.err("invalid number"));
        }
        self.digits();
        Ok(())
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::json::compact_json;

    fn roundtrip(src: &str) -> String {
        compact_json(&decode_all(src).unwrap())
    }

    #[test]
    fn escapes() {
        assert_eq!(
            roundtrip(r#""a\"b\\c\/d\b\f\n\r\teA""#),
            "\"a\\\"b\\\\c/d\\u0008\\u000c\\n\\r\\teA\""
        );
    }

    #[test]
    fn surrogate_pair() {
        assert_eq!(decode_all(r#""😀""#).unwrap(), Value::String("😀".into()));
    }

    #[test]
    fn lone_high_surrogate_becomes_replacement() {
        assert_eq!(
            decode_all(r#""\ud800x""#).unwrap(),
            Value::String("\u{FFFD}x".into())
        );
    }

    #[test]
    fn lone_low_surrogate_becomes_replacement() {
        assert_eq!(
            decode_all(r#""\udc00""#).unwrap(),
            Value::String("\u{FFFD}".into())
        );
    }

    #[test]
    fn double_high_surrogate_becomes_two_replacements() {
        assert_eq!(
            decode_all(r#""\ud800\ud800""#).unwrap(),
            Value::String("\u{FFFD}\u{FFFD}".into())
        );
    }

    #[test]
    fn high_surrogate_with_invalid_hex_follow_errors() {
        assert!(matches!(
            decode_all(r#""\ud800\uzz00""#),
            Err(Error::NotJson(_))
        ));
    }

    #[test]
    fn unescaped_control_char_errors() {
        assert!(matches!(
            decode_all("\"a\u{0001}b\""),
            Err(Error::NotJson(_))
        ));
    }

    #[test]
    fn duplicate_keys_preserved() {
        assert_eq!(roundtrip(r#"{"a":1,"a":2}"#), r#"{"a":1,"a":2}"#);
    }

    #[test]
    fn key_order_preserved() {
        assert_eq!(
            roundtrip(r#"{"zeta":1,"alpha":2}"#),
            r#"{"zeta":1,"alpha":2}"#
        );
    }

    #[test]
    fn number_lexemes_verbatim() {
        for lexeme in [
            "1e999",
            "-0",
            "3.14159265358979323846264338",
            "123456789012345678901234567890",
            "1E2",
            "0.5000",
            "-1.5e+10",
        ] {
            assert_eq!(roundtrip(lexeme), lexeme, "lexeme {lexeme}");
        }
    }

    #[test]
    fn invalid_numbers_error() {
        for src in ["1.", ".5", "1e", "1e+", "+1", "-", "0.5.5", "1e2e3", "1x"] {
            assert!(decode_all(src).is_err(), "src {src}");
        }
    }

    #[test]
    fn ndjson_folds_to_array() {
        assert_eq!(roundtrip("{\"a\":1}\n{\"b\":2}"), r#"[{"a":1},{"b":2}]"#);
        assert_eq!(roundtrip("1 2 3"), "[1,2,3]");
        assert_eq!(roundtrip(r#"{"a":1}"#), r#"{"a":1}"#);
    }

    // Adjacent top-level docs self-delimit exactly as Go's json.Decoder
    // streams them (verified against encoding/json: "01" folds to [0,1]).
    #[test]
    fn adjacent_docs_fold_like_go() {
        assert_eq!(roundtrip("01"), "[0,1]");
        assert_eq!(roundtrip("12true"), "[12,true]");
        assert_eq!(roundtrip(r#"1{"a":1}"#), r#"[1,{"a":1}]"#);
        assert_eq!(roundtrip("truefalse"), "[true,false]");
        assert_eq!(roundtrip("1-2"), "[1,-2]");
        assert_eq!(roundtrip(r#""a""b""#), r#"["a","b"]"#);
        assert_eq!(roundtrip("[]{}"), "[[],{}]");
    }

    #[test]
    fn empty_and_whitespace_error() {
        assert!(matches!(decode_all(""), Err(Error::NotJson(_))));
        assert!(matches!(decode_all("   \n  "), Err(Error::NotJson(_))));
    }

    #[test]
    fn non_json_errors() {
        assert!(matches!(
            decode_all("hello not json\n"),
            Err(Error::NotJson(_))
        ));
        assert!(matches!(decode_all(r#"{"a":1,}"#), Err(Error::NotJson(_))));
        assert!(matches!(decode_all("[1,]"), Err(Error::NotJson(_))));
    }

    // 512 is stack-safe on the default test stacks, so no custom thread: at the
    // cap parses, one past returns the clean depth error (never an abort).
    #[test]
    fn depth_cap_arrays() {
        let at_cap = "[".repeat(MAX_NESTING_DEPTH) + &"]".repeat(MAX_NESTING_DEPTH);
        assert!(decode_all(&at_cap).is_ok());
        let over_cap = "[".repeat(MAX_NESTING_DEPTH + 1) + &"]".repeat(MAX_NESTING_DEPTH + 1);
        assert!(matches!(
            decode_all(&over_cap),
            Err(Error::NotJson(msg)) if msg.contains("exceeded max nesting depth")
        ));
    }

    #[test]
    fn depth_cap_objects() {
        let open = r#"{"a":"#;
        let at_cap = open.repeat(MAX_NESTING_DEPTH) + "0" + &"}".repeat(MAX_NESTING_DEPTH);
        assert!(decode_all(&at_cap).is_ok());
        let over_cap =
            open.repeat(MAX_NESTING_DEPTH + 1) + "0" + &"}".repeat(MAX_NESTING_DEPTH + 1);
        assert!(matches!(
            decode_all(&over_cap),
            Err(Error::NotJson(msg)) if msg.contains("exceeded max nesting depth")
        ));
    }
}
