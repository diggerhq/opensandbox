#!/usr/bin/env python3
"""Entry point to run the Streamlit UI."""

import subprocess
import sys


def main():
    subprocess.run([sys.executable, "-m", "streamlit", "run", "src/ui/app.py"])


if __name__ == "__main__":
    main()
