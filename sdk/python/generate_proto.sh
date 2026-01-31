#!/bin/bash
# Generate Python protobuf files from proto/sandbox.proto

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
PROTO_DIR="$SCRIPT_DIR/../../proto"
OUT_DIR="$SCRIPT_DIR/opensandbox/proto"

python -m grpc_tools.protoc \
    -I"$PROTO_DIR" \
    --python_out="$OUT_DIR" \
    --grpc_python_out="$OUT_DIR" \
    "$PROTO_DIR/sandbox.proto"

# Fix the import path in the generated grpc file
sed -i.bak 's/import sandbox_pb2/from . import sandbox_pb2/' "$OUT_DIR/sandbox_pb2_grpc.py"
rm -f "$OUT_DIR/sandbox_pb2_grpc.py.bak"

echo "Generated Python protobuf files in $OUT_DIR"
