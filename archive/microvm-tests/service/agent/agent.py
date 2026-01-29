#!/usr/bin/env python3
"""
Sandbox Agent with Btrfs snapshots - runs inside the VM
"""

import subprocess
import os
import json
import shutil
import tarfile
import hashlib
from http.server import HTTPServer, BaseHTTPRequestHandler
from datetime import datetime

PORT = 3000
WORKSPACE = "/btrfs/workspace"  # Btrfs subvolume (real path, not symlink)
SNAPSHOTS_DIR = "/btrfs/snapshots"  # Where snapshots are stored
EXPORTS_DIR = "/btrfs/exports"  # Where exported archives are stored

# Ensure directories exist
os.makedirs(WORKSPACE, exist_ok=True)
os.makedirs(SNAPSHOTS_DIR, exist_ok=True)
os.makedirs(EXPORTS_DIR, exist_ok=True)


def run_cmd(cmd, timeout=30, cwd=None):
    """Run a shell command and return result"""
    try:
        result = subprocess.run(
            cmd,
            shell=True,
            capture_output=True,
            text=True,
            timeout=timeout,
            cwd=cwd or WORKSPACE,
        )
        return {
            "stdout": result.stdout,
            "stderr": result.stderr,
            "exitCode": result.returncode,
        }
    except subprocess.TimeoutExpired:
        return {"stdout": "", "stderr": "Command timed out", "exitCode": 124}
    except Exception as e:
        return {"stdout": "", "stderr": str(e), "exitCode": 1}


def take_snapshot(name):
    """Take a Btrfs snapshot of the workspace"""
    snapshot_path = os.path.join(SNAPSHOTS_DIR, name)
    
    # Delete if exists (for overwrites)
    if os.path.exists(snapshot_path):
        subprocess.run(f"sudo btrfs subvolume delete {snapshot_path}", shell=True)
    
    result = subprocess.run(
        f"sudo btrfs subvolume snapshot {WORKSPACE} {snapshot_path}",
        shell=True,
        capture_output=True,
        text=True,
    )
    
    if result.returncode != 0:
        raise Exception(f"Snapshot failed: {result.stderr}")
    
    return snapshot_path


def restore_snapshot(name):
    """Restore workspace from a Btrfs snapshot"""
    snapshot_path = os.path.join(SNAPSHOTS_DIR, name)
    
    if not os.path.exists(snapshot_path):
        raise Exception(f"Snapshot '{name}' not found")
    
    # Delete current workspace subvolume
    subprocess.run(f"sudo btrfs subvolume delete {WORKSPACE}", shell=True)
    
    # Create new snapshot from the saved one
    result = subprocess.run(
        f"sudo btrfs subvolume snapshot {snapshot_path} {WORKSPACE}",
        shell=True,
        capture_output=True,
        text=True,
    )
    
    if result.returncode != 0:
        raise Exception(f"Restore failed: {result.stderr}")
    
    # Fix permissions
    subprocess.run(f"sudo chown -R $(whoami):$(whoami) {WORKSPACE}", shell=True)


def list_snapshots():
    """List all snapshots"""
    snapshots = []
    if os.path.exists(SNAPSHOTS_DIR):
        for name in sorted(os.listdir(SNAPSHOTS_DIR)):
            path = os.path.join(SNAPSHOTS_DIR, name)
            stat = os.stat(path)
            snapshots.append({
                "name": name,
                "created_at": datetime.fromtimestamp(stat.st_ctime).isoformat(),
            })
    return snapshots


def delete_snapshot(name):
    """Delete a snapshot"""
    snapshot_path = os.path.join(SNAPSHOTS_DIR, name)
    if os.path.exists(snapshot_path):
        subprocess.run(f"sudo btrfs subvolume delete {snapshot_path}", shell=True)


def export_snapshot_to_file(name):
    """Export a snapshot to a local tar.gz file"""
    snapshot_path = os.path.join(SNAPSHOTS_DIR, name)
    if not os.path.exists(snapshot_path):
        if name == "workspace":
            snapshot_path = WORKSPACE
        else:
            raise Exception(f"Snapshot '{name}' not found")
    
    export_name = f"{name}_{int(datetime.now().timestamp())}.tar.gz"
    export_path = os.path.join(EXPORTS_DIR, export_name)
    
    with tarfile.open(export_path, 'w:gz') as tar:
        tar.add(snapshot_path, arcname='workspace')
    
    size = os.path.getsize(export_path)
    
    # Calculate hash for integrity
    with open(export_path, 'rb') as f:
        file_hash = hashlib.sha256(f.read()).hexdigest()
    
    return {"path": export_path, "name": export_name, "size": size, "sha256": file_hash}


def import_from_file(name, file_path):
    """Import a snapshot from a local tar.gz file"""
    snapshot_path = os.path.join(SNAPSHOTS_DIR, name)
    
    # Delete if exists
    if os.path.exists(snapshot_path):
        subprocess.run(f"sudo btrfs subvolume delete {snapshot_path}", shell=True)
    
    # Create new subvolume
    subprocess.run(f"sudo btrfs subvolume create {snapshot_path}", shell=True, check=True)
    
    # Extract
    with tarfile.open(file_path, 'r:gz') as tar:
        tar.extractall(snapshot_path, filter='data')
    
    # Move contents from workspace subfolder to root
    workspace_subdir = os.path.join(snapshot_path, 'workspace')
    if os.path.exists(workspace_subdir):
        for item in os.listdir(workspace_subdir):
            shutil.move(os.path.join(workspace_subdir, item), snapshot_path)
        os.rmdir(workspace_subdir)
    
    # Fix permissions
    subprocess.run(f"sudo chown -R $(whoami):$(whoami) {snapshot_path}", shell=True)
    
    return snapshot_path


def list_exports():
    """List all exported archives"""
    exports = []
    if os.path.exists(EXPORTS_DIR):
        for name in sorted(os.listdir(EXPORTS_DIR)):
            path = os.path.join(EXPORTS_DIR, name)
            stat = os.stat(path)
            exports.append({
                "name": name,
                "size": stat.st_size,
                "created_at": datetime.fromtimestamp(stat.st_ctime).isoformat(),
            })
    return exports


class AgentHandler(BaseHTTPRequestHandler):
    def _send_json(self, data, status=200):
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.end_headers()
        self.wfile.write(json.dumps(data).encode())

    def _read_body(self):
        length = int(self.headers.get("Content-Length", 0))
        if length:
            return json.loads(self.rfile.read(length))
        return {}

    def do_GET(self):
        if self.path == "/health":
            self._send_json({
                "status": "ok",
                "workspace": WORKSPACE,
                "snapshots_dir": SNAPSHOTS_DIR,
                "pid": os.getpid(),
            })
        elif self.path == "/snapshots":
            self._send_json({"snapshots": list_snapshots()})
        elif self.path == "/exports":
            self._send_json({"exports": list_exports()})
        elif self.path == "/workspace":
            # List workspace contents
            try:
                files = os.listdir(WORKSPACE)
                self._send_json({"files": files})
            except Exception as e:
                self._send_json({"error": str(e)}, 500)
        elif self.path.startswith("/download/"):
            # Stream a file from exports directory
            filename = self.path.replace("/download/", "")
            file_path = os.path.join(EXPORTS_DIR, filename)
            if not os.path.exists(file_path):
                self._send_json({"error": "File not found"}, 404)
                return
            try:
                self.send_response(200)
                self.send_header("Content-Type", "application/gzip")
                self.send_header("Content-Length", os.path.getsize(file_path))
                self.send_header("Content-Disposition", f'attachment; filename="{filename}"')
                self.end_headers()
                with open(file_path, 'rb') as f:
                    shutil.copyfileobj(f, self.wfile)
                # Clean up after download
                os.remove(file_path)
            except Exception as e:
                self._send_json({"error": str(e)}, 500)
        else:
            self._send_json({"error": "Not found"}, 404)

    def do_PUT(self):
        """Handle file uploads for import"""
        if self.path.startswith("/upload/"):
            snapshot_name = self.path.replace("/upload/", "")
            try:
                # Read the file data
                content_length = int(self.headers.get("Content-Length", 0))
                file_path = os.path.join(EXPORTS_DIR, f"upload_{snapshot_name}.tar.gz")
                
                # Stream to file
                with open(file_path, 'wb') as f:
                    remaining = content_length
                    while remaining > 0:
                        chunk_size = min(8192, remaining)
                        chunk = self.rfile.read(chunk_size)
                        if not chunk:
                            break
                        f.write(chunk)
                        remaining -= len(chunk)
                
                # Import the snapshot
                path = import_from_file(snapshot_name, file_path)
                
                # Clean up
                os.remove(file_path)
                
                self._send_json({"message": f"Imported snapshot {snapshot_name}", "path": path})
            except Exception as e:
                self._send_json({"error": str(e)}, 500)
        else:
            self._send_json({"error": "Not found"}, 404)

    def do_POST(self):
        body = self._read_body()

        if self.path == "/exec":
            cmd = body.get("command")
            timeout = body.get("timeout", 30)
            if not cmd:
                self._send_json({"error": "command required"}, 400)
                return
            result = run_cmd(cmd, timeout)
            self._send_json(result)

        elif self.path == "/snapshot":
            name = body.get("name", f"snap_{int(datetime.now().timestamp() * 1000)}")
            try:
                path = take_snapshot(name)
                self._send_json({"name": name, "path": path})
            except Exception as e:
                self._send_json({"error": str(e)}, 500)

        elif self.path == "/restore":
            name = body.get("name")
            if not name:
                self._send_json({"error": "name required"}, 400)
                return
            try:
                restore_snapshot(name)
                self._send_json({"message": f"Restored to {name}"})
            except Exception as e:
                self._send_json({"error": str(e)}, 500)

        elif self.path == "/wipe":
            # Wipe workspace contents (but keep the subvolume)
            for item in os.listdir(WORKSPACE):
                path = os.path.join(WORKSPACE, item)
                if os.path.isdir(path):
                    shutil.rmtree(path)
                else:
                    os.remove(path)
            self._send_json({"message": "Workspace wiped"})

        elif self.path == "/snapshot/delete":
            name = body.get("name")
            if not name:
                self._send_json({"error": "name required"}, 400)
                return
            try:
                delete_snapshot(name)
                self._send_json({"message": f"Deleted snapshot {name}"})
            except Exception as e:
                self._send_json({"error": str(e)}, 500)

        elif self.path == "/export":
            # Export snapshot to local file, return metadata (use GET /download/:name to fetch)
            name = body.get("name", "workspace")
            try:
                result = export_snapshot_to_file(name)
                self._send_json(result)
            except Exception as e:
                self._send_json({"error": str(e)}, 500)

        else:
            self._send_json({"error": "Not found"}, 404)

    def log_message(self, format, *args):
        print(f"[Agent] {args[0]}")


if __name__ == "__main__":
    print(f"🚀 Sandbox agent starting on port {PORT}")
    print(f"   Workspace: {WORKSPACE}")
    print(f"   Snapshots: {SNAPSHOTS_DIR}")
    server = HTTPServer(("0.0.0.0", PORT), AgentHandler)
    server.serve_forever()
