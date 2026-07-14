//! Differential fuzz: our hand-rolled decoder + compact writer against
//! serde_json on serde_json-generatable inputs. Asserts lexeme-level identity
//! (serde_json's rendering round-trips byte-for-byte through us) and semantic
//! equality of our output re-parsed by serde_json.

use format_core::{encode_as, Format, SelectOpts};
use proptest::prelude::*;
use serde_json::{json, Map, Value};

fn arb_json() -> impl Strategy<Value = Value> {
    let leaf = prop_oneof![
        Just(Value::Null),
        any::<bool>().prop_map(Value::Bool),
        any::<i64>().prop_map(|i| json!(i)),
        any::<u64>().prop_map(|u| json!(u)),
        any::<f64>()
            .prop_filter("finite", |f| f.is_finite())
            .prop_map(|f| json!(f)),
        "\\PC*".prop_map(Value::String),
    ];
    leaf.prop_recursive(4, 64, 8, |inner| {
        prop_oneof![
            prop::collection::vec(inner.clone(), 0..8).prop_map(Value::Array),
            prop::collection::vec(("\\PC*", inner), 0..8)
                .prop_map(|kvs| { Value::Object(kvs.into_iter().collect::<Map<String, Value>>()) }),
        ]
    })
}

proptest! {
    #[test]
    fn decode_compact_roundtrip_vs_serde_json(v in arb_json()) {
        let src = serde_json::to_string(&v).unwrap();
        let out = encode_as(&src, Format::Json, &SelectOpts::default()).unwrap();
        // Lexeme preservation, modulo the one intentional divergence: the writer
        // always escapes U+2028/U+2029 (Go encoding/json parity); serde_json
        // emits them raw.
        let expected = src.replace('\u{2028}', "\\u2028").replace('\u{2029}', "\\u2029");
        prop_assert_eq!(&out, &expected);
        // Semantic equality through the oracle.
        let reparsed: Value = serde_json::from_str(&out).unwrap();
        prop_assert_eq!(reparsed, v);
    }

    #[test]
    fn ndjson_folding_matches_serde_json_stream(docs in prop::collection::vec(arb_json(), 2..5)) {
        let src = docs.iter().map(|d| serde_json::to_string(d).unwrap()).collect::<Vec<_>>().join("\n");
        let out = encode_as(&src, Format::Json, &SelectOpts::default()).unwrap();
        let reparsed: Value = serde_json::from_str(&out).unwrap();
        prop_assert_eq!(reparsed, Value::Array(docs));
    }
}
