package logging

import (
    "fmt"
    "log/slog"
    "os"
    "path/filepath"
    "strings"
    "time"
    "io"
)

var Logger *slog.Logger

// Init initialise le logger structuré.
// format: "json" ou "text"
// level: debug|info|warn|error
type syncFileWriter struct { f *os.File; sync bool }
func (w *syncFileWriter) Write(p []byte) (int, error) {
    if w.f == nil { return len(p), nil }
    n, err := w.f.Write(p)
    if err == nil && w.sync { _ = w.f.Sync() }
    return n, err
}

// filePath optionnel. Si fourni: crée/append dans ce fichier après rotation (renommage fichier existant avec timestamp au démarrage).
// syncWrites: si true force fsync après chaque écriture (ralentit mais garantit la persistance immédiate).
func Init(format, level string, filePath string, syncWrites bool) {
    lvlVar := new(slog.LevelVar)
    switch strings.ToLower(level) {
    case "debug": lvlVar.Set(slog.LevelDebug)
    case "warn": lvlVar.Set(slog.LevelWarn)
    case "error": lvlVar.Set(slog.LevelError)
    default: lvlVar.Set(slog.LevelInfo)
    }
    var writers []io.Writer
    writers = append(writers, os.Stdout)
    if filePath != "" {
        // rotation simple par redémarrage
        if st, err := os.Stat(filePath); err == nil && st.Size() > 0 {
            ts := time.Now().Format("20060102-150405")
            rotated := fmt.Sprintf("%s.%s", filePath, ts)
            _ = os.Rename(filePath, rotated)
        }
        if err := os.MkdirAll(filepath.Dir(filePath), 0o755); err == nil {
            f, err := os.OpenFile(filePath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
            if err == nil { writers = append(writers, &syncFileWriter{f: f, sync: syncWrites}) }
        }
    }
    mw := io.MultiWriter(writers...)
    var h slog.Handler
    if strings.ToLower(format) == "json" {
        h = slog.NewJSONHandler(mw, &slog.HandlerOptions{Level: lvlVar})
    } else {
        h = slog.NewTextHandler(mw, &slog.HandlerOptions{Level: lvlVar})
    }
    Logger = slog.New(h)
    // Définit ce logger comme défaut pour que slog.Info/... écrive dans le fichier choisi.
    slog.SetDefault(Logger)
}
