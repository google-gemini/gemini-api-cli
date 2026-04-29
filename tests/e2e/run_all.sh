#!/bin/bash
source ~/.bash_profile
echo "Running all E2E tests..."
for f in tests/e2e/cuj_*.sh; do
  echo "----------------------------------------"
  echo "Running $f"
  echo "----------------------------------------"
  bash "$f"
  if [ $? -ne 0 ]; then
    echo "FAILED: $f"
  fi
done
