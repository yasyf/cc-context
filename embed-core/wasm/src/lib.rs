//! WASM (`wasm32-unknown-unknown`) adapter over [`model2vec_rs`]: a *resident*
//! static-embedding engine.
//!
//! Unlike format-core's one-shot modules, this instance is instantiated once
//! and kept alive: [`em_load_model`] parses the model blobs into a process-wide
//! [`StaticModel`] that every later [`em_encode`] call reuses, so the ~30 MB of
//! weights are decoded exactly once. The module has zero host imports (no WASI,
//! no I/O) and, under `panic = "abort"`, traps on any host-contract violation —
//! the host treats a trap as an error.
//!
//! ## ABI
//!
//! Memory ownership is symmetric: the host allocates every input buffer with
//! [`em_alloc`] and frees it with [`em_dealloc`]; the module allocates every
//! output buffer and hands back its `(ptr << 32) | len`, which the host reads
//! and then frees with [`em_dealloc`]. All integers are little-endian.
//!
//! - `em_alloc(len) -> ptr`
//! - `em_dealloc(ptr, len)`
//! - `em_load_model(tok_ptr, tok_len, model_ptr, model_len, cfg_ptr, cfg_len) -> packed`
//!   — `(0 << 32) | 0` on success; otherwise a `(ptr << 32) | len` UTF-8 error
//!   message the host reads and frees.
//! - `em_encode(batch_ptr, batch_len) -> packed` — one flat copy-out matrix.
//!
//! Batch input framing: `[u32 count]` then, per text, `[u32 byte_len][utf8]`.
//! Encode output framing: `[u32 rows][u32 dims]` then `rows * dims` `f32`s,
//! row-major.

#![cfg_attr(not(test), deny(clippy::unwrap_used, clippy::expect_used))]

use std::alloc::{alloc, dealloc, Layout};
use std::cell::RefCell;
use std::ptr::copy_nonoverlapping;

use model2vec_rs::model::StaticModel;

thread_local! {
    /// The resident model, populated by [`em_load_model`]. wasm32 is
    /// single-threaded and the host serializes calls, so a thread-local
    /// `RefCell` is the whole synchronization story.
    static MODEL: RefCell<Option<StaticModel>> = const { RefCell::new(None) };
}

/// getrandom's `custom` backend hook (selected in `../.cargo/config.toml`).
/// Every getrandom call in the graph — ahash/rand HashMap seeding — routes
/// here. Inference is deterministic and never needs entropy, so a fixed-seed
/// splitmix64 fill keeps the module free of the `wasm_js` JS imports while
/// still handing HashMap a well-distributed seed.
#[no_mangle]
unsafe extern "Rust" fn __getrandom_v03_custom(dest: *mut u8, len: usize) -> Result<(), getrandom::Error> {
    let mut state: u64 = 0x9E37_79B9_7F4A_7C15;
    let mut i = 0;
    while i < len {
        state = state.wrapping_add(0x9E37_79B9_7F4A_7C15);
        let mut z = state;
        z = (z ^ (z >> 30)).wrapping_mul(0xBF58_476D_1CE4_E5B9);
        z = (z ^ (z >> 27)).wrapping_mul(0x94D0_49BB_1331_11EB);
        z ^= z >> 31;
        let bytes = z.to_le_bytes();
        let take = (len - i).min(8);
        core::ptr::copy_nonoverlapping(bytes.as_ptr(), dest.add(i), take);
        i += take;
    }
    Ok(())
}

fn pack(ptr: u32, len: u32) -> u64 {
    ((ptr as u64) << 32) | len as u64
}

/// Copies `data` into a freshly allocated, exact-capacity buffer and returns
/// its `(ptr, len)`. Exact capacity is what lets the host free it with a
/// matching [`Layout`] in [`em_dealloc`]; a zero-length payload returns a
/// non-null dangling pointer the host never reads.
fn leak_bytes(data: &[u8]) -> u64 {
    if data.is_empty() {
        return pack(1, 0);
    }
    let layout = layout_for(data.len());
    let ptr = unsafe { alloc(layout) };
    if ptr.is_null() {
        std::alloc::handle_alloc_error(layout);
    }
    unsafe { copy_nonoverlapping(data.as_ptr(), ptr, data.len()) };
    pack(ptr as u32, data.len() as u32)
}

fn layout_for(len: usize) -> Layout {
    // Alignment 1: the host reads raw bytes and interprets them itself, and
    // every alloc/dealloc pair agrees on this alignment.
    match Layout::from_size_align(len, 1) {
        Ok(l) => l,
        Err(_) => std::process::abort(),
    }
}

unsafe fn borrow<'a>(ptr: u32, len: u32) -> &'a [u8] {
    core::slice::from_raw_parts(ptr as usize as *const u8, len as usize)
}

fn read_u32(buf: &[u8], off: usize) -> u32 {
    u32::from_le_bytes([buf[off], buf[off + 1], buf[off + 2], buf[off + 3]])
}

fn parse_batch(buf: &[u8]) -> Vec<String> {
    let count = read_u32(buf, 0) as usize;
    let mut texts = Vec::with_capacity(count);
    let mut off = 4;
    for _ in 0..count {
        let blen = read_u32(buf, off) as usize;
        off += 4;
        let bytes = &buf[off..off + blen];
        off += blen;
        match core::str::from_utf8(bytes) {
            Ok(s) => texts.push(s.to_owned()),
            Err(_) => panic!("embed-wasm: batch text is not valid UTF-8"),
        }
    }
    texts
}

/// Allocates a `len`-byte buffer for the host to write into and returns its
/// address. Paired with [`em_dealloc`].
#[no_mangle]
pub extern "C" fn em_alloc(len: u32) -> u32 {
    if len == 0 {
        return 1;
    }
    let ptr = unsafe { alloc(layout_for(len as usize)) };
    if ptr.is_null() {
        std::alloc::handle_alloc_error(layout_for(len as usize));
    }
    ptr as u32
}

/// Frees a buffer previously returned by [`em_alloc`] or packed out of
/// [`em_load_model`] / [`em_encode`]. `len` must be the same length that
/// produced the pointer.
#[no_mangle]
pub extern "C" fn em_dealloc(ptr: u32, len: u32) {
    if len == 0 {
        return;
    }
    unsafe { dealloc(ptr as usize as *mut u8, layout_for(len as usize)) };
}

/// Parses the three model blobs into the resident [`StaticModel`], honoring the
/// `normalize` field in `config.json` (default `true`). Returns `(0, 0)` on
/// success or a packed `(ptr, len)` UTF-8 error message the host frees.
///
/// The host owns the three input buffers and frees them after this returns.
#[no_mangle]
pub extern "C" fn em_load_model(
    tok_ptr: u32,
    tok_len: u32,
    model_ptr: u32,
    model_len: u32,
    cfg_ptr: u32,
    cfg_len: u32,
) -> u64 {
    let tok = unsafe { borrow(tok_ptr, tok_len) };
    let model = unsafe { borrow(model_ptr, model_len) };
    let cfg = unsafe { borrow(cfg_ptr, cfg_len) };
    match StaticModel::from_bytes(tok, model, cfg, None) {
        Ok(m) => {
            MODEL.with(|cell| *cell.borrow_mut() = Some(m));
            pack(0, 0)
        }
        Err(e) => leak_bytes(e.to_string().as_bytes()),
    }
}

/// Encodes the framed batch at `ptr`/`len` into a flat row-major `f32` matrix,
/// returned as a packed `(ptr, len)` the host reads and frees. Encoding uses no
/// token truncation (`max_length = None`) so long code chunks embed in full.
///
/// The host owns the input buffer and frees it after this returns. Calling this
/// before [`em_load_model`] is a host-contract violation and traps.
#[no_mangle]
pub extern "C" fn em_encode(ptr: u32, len: u32) -> u64 {
    let buf = unsafe { borrow(ptr, len) };
    let texts = parse_batch(buf);
    let vectors = MODEL.with(|cell| {
        let guard = cell.borrow();
        let model = match guard.as_ref() {
            Some(m) => m,
            None => panic!("embed-wasm: em_encode called before em_load_model"),
        };
        // batch_size = 1 is deliberate: this model's tokenizer.json enables
        // BatchLongest padding (pad_id 0), and model2vec's mean-pool does not
        // mask pad tokens, so any multi-text batch pads short texts up to the
        // batch's longest and pollutes their embeddings — making the output
        // depend on batch composition. Encoding one text per tokenizer batch
        // pads each to itself (no pad tokens), yielding batch-invariant vectors
        // while a single em_encode still amortizes the ABI crossing over the
        // whole batch. max_length None: never truncate long code chunks.
        model.encode_with_args(&texts, None, 1)
    });

    let rows = vectors.len();
    let dims = vectors.first().map_or(0, Vec::len);
    let mut out = Vec::with_capacity(8 + rows * dims * 4);
    out.extend_from_slice(&(rows as u32).to_le_bytes());
    out.extend_from_slice(&(dims as u32).to_le_bytes());
    for v in &vectors {
        for &f in v {
            out.extend_from_slice(&f.to_le_bytes());
        }
    }
    leak_bytes(&out)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn pack_splits_into_ptr_and_len() {
        let packed = pack(0xDEAD_BEEF, 0x0000_1234);
        assert_eq!((packed >> 32) as u32, 0xDEAD_BEEF);
        assert_eq!(packed as u32, 0x0000_1234);
    }

    // em_alloc/em_dealloc are exercised end-to-end by the Go tests: their u32
    // pointers only round-trip under wasm32, so a host-side alloc/dealloc test
    // would truncate a 64-bit pointer and free garbage.

    #[test]
    fn parse_batch_reads_framed_texts() {
        let mut buf = Vec::new();
        let texts = ["hello", "", "wörld"];
        buf.extend_from_slice(&(texts.len() as u32).to_le_bytes());
        for t in texts {
            buf.extend_from_slice(&(t.len() as u32).to_le_bytes());
            buf.extend_from_slice(t.as_bytes());
        }
        assert_eq!(parse_batch(&buf), vec!["hello", "", "wörld"]);
    }
}
