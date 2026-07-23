// Force-included when building the tree-sitter runtime for wasm32-wasi, whose
// wasi-libc <endian.h> lacks the be*toh/htobe* family. Defining ENDIAN_H first
// makes tree-sitter's portable/endian.h a no-op. wasm32 is little-endian.
#ifndef ENDIAN_H
#define ENDIAN_H
#define htobe16(x) __builtin_bswap16(x)
#define htole16(x) (x)
#define be16toh(x) __builtin_bswap16(x)
#define le16toh(x) (x)
#define htobe32(x) __builtin_bswap32(x)
#define htole32(x) (x)
#define be32toh(x) __builtin_bswap32(x)
#define le32toh(x) (x)
#define htobe64(x) __builtin_bswap64(x)
#define htole64(x) (x)
#define be64toh(x) __builtin_bswap64(x)
#define le64toh(x) (x)
#endif
