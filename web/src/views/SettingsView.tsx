import { useEffect, useState, useCallback } from "react";
import { AlertCircle, Sun, Moon, Monitor } from "lucide-react";
import { fetchConfig, patchConfig } from "../api/client";
import type { ServerConfig } from "../api/client";
import { useThemeStore, toggleTheme } from "../stores/ui";
import { Card, CardContent, CardHeader, CardTitle } from "../components/ui/card";
import { Label } from "../components/ui/label";
import { Input } from "../components/ui/input";
import { Button } from "../components/ui/button";
import { Skeleton } from "../components/ui/skeleton";
import { Alert, AlertDescription, AlertTitle } from "../components/ui/alert";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "../components/ui/select";

export default function SettingsView() {
  const theme = useThemeStore((s) => s.theme);
  const [config, setConfig] = useState<ServerConfig | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [patchable, setPatchable] = useState(false);
  const [saving, setSaving] = useState(false);
  const [saveMsg, setSaveMsg] = useState<string | null>(null);
  const [logLevel, setLogLevel] = useState("");

  const load = useCallback(async () => {
    try {
      const data = await fetchConfig();
      setConfig(data);
      setLogLevel(data.log_level ?? "info");
      setError(null);

      // Test if PATCH is supported by checking if we can construct a request
      // We'll try a no-op patch; if it fails with 403/405, read-only
      try {
        await patchConfig({ log_level: data.log_level ?? "info" });
        setPatchable(true);
      } catch {
        setPatchable(false);
      }
    } catch (err: unknown) {
      const message =
        err instanceof Error ? err.message : "Failed to fetch config";
      setError(message);
    } finally {
      setLoading(false);
    }
  }, []);

  useEffect(() => {
    load();
  }, [load]);

  const handleSaveLogLevel = useCallback(
    async (level: string) => {
      if (!patchable) return;
      setSaving(true);
      setSaveMsg(null);
      try {
        const result = await patchConfig({ log_level: level });
        setConfig(result.config);
        setLogLevel(level);
        if (result.restart_required && result.restart_required.length > 0) {
          setSaveMsg(
            `Saved. Restart required for: ${result.restart_required.join(", ")}`,
          );
        } else {
          setSaveMsg("Log level updated.");
        }
      } catch (err: unknown) {
        const message =
          err instanceof Error ? err.message : "Failed to update config";
        setSaveMsg(message);
      } finally {
        setSaving(false);
      }
    },
    [patchable],
  );

  // Loading skeleton
  if (loading) {
    return (
      <div className="mx-auto max-w-[900px] p-6">
        <h1 className="mb-6 text-lg font-semibold text-foreground">
          Settings
        </h1>
        <div className="flex flex-col gap-4">
          {Array.from({ length: 3 }).map((_, i) => (
            <Skeleton key={i} className="h-32 w-full rounded-md" />
          ))}
        </div>
      </div>
    );
  }

  // Error state
  if (error && !config) {
    return (
      <div className="flex flex-col items-center justify-center gap-3 p-16">
        <Alert variant="destructive" className="max-w-md rounded-md">
          <AlertCircle className="size-4" />
          <AlertTitle>Failed to load settings</AlertTitle>
          <AlertDescription>{error}</AlertDescription>
        </Alert>
        <Button variant="outline" size="sm" onClick={load}>
          Retry
        </Button>
      </div>
    );
  }

  return (
    <div className="mx-auto max-w-[900px] p-6">
      <h1 className="mb-6 text-lg font-semibold text-foreground">Settings</h1>

      <div className="flex flex-col gap-4">
        {/* Theme card */}
        <Card className="rounded-md shadow-none">
          <CardHeader className="pb-0">
            <CardTitle className="text-sm">Appearance</CardTitle>
          </CardHeader>
          <CardContent>
            <div className="flex items-center gap-4">
              <Label className="min-w-[80px] text-[0.8125rem] text-muted-foreground">
                Theme
              </Label>
              <div className="flex items-center gap-2">
                <Button
                  variant={theme === "light" ? "default" : "outline"}
                  size="sm"
                  onClick={() => {
                    if (theme !== "light") toggleTheme();
                  }}
                >
                  <Sun className="size-3.5" />
                  Light
                </Button>
                <Button
                  variant={theme === "dark" ? "default" : "outline"}
                  size="sm"
                  onClick={() => {
                    if (theme !== "dark") toggleTheme();
                  }}
                >
                  <Moon className="size-3.5" />
                  Dark
                </Button>
              </div>
            </div>
          </CardContent>
        </Card>

        {/* Server config card */}
        {config && (
          <Card className="rounded-md shadow-none">
            <CardHeader className="pb-0">
              <CardTitle className="text-sm">Server Configuration</CardTitle>
            </CardHeader>
            <CardContent>
              <div className="flex flex-col gap-3">
                <div className="flex items-center gap-4">
                  <Label className="min-w-[140px] text-[0.8125rem] text-muted-foreground">
                    Listen Address
                  </Label>
                  <Input
                    value={config.listen ?? "--"}
                    readOnly
                    className="max-w-[300px] bg-muted text-sm"
                  />
                </div>

                <div className="flex items-center gap-4">
                  <Label className="min-w-[140px] text-[0.8125rem] text-muted-foreground">
                    Data Directory
                  </Label>
                  <Input
                    value={config.data_dir ?? "(in-memory)"}
                    readOnly
                    className="max-w-[300px] bg-muted font-mono text-xs"
                  />
                </div>

                <div className="flex items-center gap-4">
                  <Label className="min-w-[140px] text-[0.8125rem] text-muted-foreground">
                    Retention
                  </Label>
                  <Input
                    value={config.retention ?? "--"}
                    readOnly
                    className="max-w-[300px] bg-muted text-sm"
                  />
                </div>

                <div className="flex items-center gap-4">
                  <Label className="min-w-[140px] text-[0.8125rem] text-muted-foreground">
                    Log Level
                  </Label>
                  {patchable ? (
                    <div className="flex items-center gap-2">
                      <Select
                        value={logLevel}
                        onValueChange={(val) => {
                          setLogLevel(val);
                          handleSaveLogLevel(val);
                        }}
                        disabled={saving}
                      >
                        <SelectTrigger className="w-[140px]">
                          <SelectValue />
                        </SelectTrigger>
                        <SelectContent>
                          <SelectItem value="debug">debug</SelectItem>
                          <SelectItem value="info">info</SelectItem>
                          <SelectItem value="warn">warn</SelectItem>
                          <SelectItem value="error">error</SelectItem>
                        </SelectContent>
                      </Select>
                      {saving && (
                        <span className="text-xs text-muted-foreground">
                          Saving...
                        </span>
                      )}
                    </div>
                  ) : (
                    <Input
                      value={config.log_level ?? "--"}
                      readOnly
                      className="max-w-[300px] bg-muted text-sm"
                    />
                  )}
                </div>

                {saveMsg && (
                  <p className="text-xs text-muted-foreground">{saveMsg}</p>
                )}
              </div>
            </CardContent>
          </Card>
        )}

        {/* Query config card */}
        {config?.query && (
          <Card className="rounded-md shadow-none">
            <CardHeader className="pb-0">
              <CardTitle className="text-sm">Query Limits</CardTitle>
            </CardHeader>
            <CardContent>
              <div className="flex flex-col gap-3">
                <div className="flex items-center gap-4">
                  <Label className="min-w-[140px] text-[0.8125rem] text-muted-foreground">
                    Max Concurrent
                  </Label>
                  <Input
                    value={String(config.query.max_concurrent ?? "--")}
                    readOnly
                    className="max-w-[120px] bg-muted text-sm"
                  />
                </div>
                <div className="flex items-center gap-4">
                  <Label className="min-w-[140px] text-[0.8125rem] text-muted-foreground">
                    Default Limit
                  </Label>
                  <Input
                    value={String(config.query.default_result_limit ?? "--")}
                    readOnly
                    className="max-w-[120px] bg-muted text-sm"
                  />
                </div>
                <div className="flex items-center gap-4">
                  <Label className="min-w-[140px] text-[0.8125rem] text-muted-foreground">
                    Max Limit
                  </Label>
                  <Input
                    value={String(config.query.max_result_limit ?? "--")}
                    readOnly
                    className="max-w-[120px] bg-muted text-sm"
                  />
                </div>
              </div>
            </CardContent>
          </Card>
        )}

        {!patchable && config && (
          <p className="text-xs text-muted-foreground">
            <Monitor className="mb-0.5 inline size-3" /> Server configuration is
            read-only. Log level and retention can be changed via{" "}
            <code className="rounded bg-muted px-1 py-0.5 font-mono text-[0.7rem]">
              PATCH /api/v1/config
            </code>{" "}
            with admin credentials, or via{" "}
            <code className="rounded bg-muted px-1 py-0.5 font-mono text-[0.7rem]">
              lynxdb config reload
            </code>.
          </p>
        )}
      </div>
    </div>
  );
}
