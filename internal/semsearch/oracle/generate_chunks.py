from collections import Counter

from semble.index.files import FileStatus, detect_language, get_extensions, get_file_status
from semble.types import ContentType

from common import CORPUS_DIR, OracleIndex, load_oracle_index, write_golden


def generate(index: OracleIndex) -> dict:
    chunk_counts = Counter(chunk.file_path for chunk in index.chunks)
    extensions = set(get_extensions((ContentType.CODE, ContentType.DOCS, ContentType.CONFIG)))
    files = []
    for path in sorted(path for path in CORPUS_DIR.rglob("*") if path.is_file()):
        file_path = path.relative_to(CORPUS_DIR).as_posix()
        status = get_file_status(path, None)
        if path.suffix.lower() not in extensions:
            classification = "unsupported_content_type"
        elif status != FileStatus.VALID:
            classification = status.value
        elif chunk_counts[file_path]:
            classification = "indexed"
        else:
            classification = "no_chunks"
        files.append(
            {
                "file_path": file_path,
                "language": detect_language(path),
                "bytes": path.stat().st_size,
                "classification": classification,
                "chunk_count": chunk_counts[file_path],
            }
        )
    return {
        "chunks": [
            {
                "path": chunk.file_path,
                "start_line": chunk.start_line,
                "end_line": chunk.end_line,
            }
            for chunk in index.chunks
        ],
        "files": files,
    }


def main() -> None:
    write_golden("chunks.json", generate(load_oracle_index()))


if __name__ == "__main__":
    main()
