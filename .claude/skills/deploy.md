---
description: Build and deploy the crush binary to system npm directory
---

Build the Go backend, bundle the frontend, kill all running crush processes, and install the new binary to the system npm directory.

## Steps

1. Build the frontend bundle:
   ```bash
   cd D:/dev/go/crush/c/web && npm run build
   ```

2. Build the Go binary:
   ```bash
   cd D:/dev/go/crush/c && go build -o crush.exe .
   ```

3. Kill all running crush processes:
   ```bash
   taskkill /F /IM crush.exe 2>nul || echo "No crush processes found"
   ```

4. Copy the new binary to npm directory (using `where crush` to find location):
   ```bash
   cd D:/dev/go/crush/c && cp -f crush.exe C:/Users/Computer/AppData/Roaming/npm/crush.exe
   ```

5. Verify the deployment:
   ```bash
   crush --version
   ```

## Usage

Run this skill whenever you make changes to the Go backend or frontend and want to test them immediately:
```bash
/deploy
```
