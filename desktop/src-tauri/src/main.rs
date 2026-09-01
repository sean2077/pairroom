// Prevents an additional console window on Windows release builds.
#![cfg_attr(not(debug_assertions), windows_subsystem = "windows")]

use serde::Deserialize;
use std::{
    collections::VecDeque,
    net::IpAddr,
    path::PathBuf,
    str::FromStr,
    sync::{
        atomic::{AtomicBool, AtomicU64, Ordering},
        Mutex,
    },
    time::Duration,
};
use tauri::{
    menu::{Menu, MenuItem},
    tray::{MouseButton, MouseButtonState, TrayIconBuilder, TrayIconEvent},
    AppHandle, Manager, RunEvent, WebviewUrl, WebviewWindowBuilder, WindowEvent,
};
use tauri_plugin_shell::{
    process::{CommandChild, CommandEvent},
    ShellExt,
};
use tokio::sync::{Mutex as AsyncMutex, Notify};
use url::Url;

const MAIN_WINDOW: &str = "main";
const SERVICE_START_TIMEOUT: Duration = Duration::from_secs(20);
const SERVICE_READY_TIMEOUT: Duration = Duration::from_secs(5);
const SERVICE_DRAIN_TIMEOUT: Duration = Duration::from_secs(11 * 60);

struct DesktopState {
    bootstrap: AsyncMutex<()>,
    child: Mutex<Option<CommandChild>>,
    control_file: Mutex<Option<PathBuf>>,
    management_url: Mutex<Option<String>>,
    owned_service: AtomicBool,
    service_terminated: AtomicBool,
    service_terminated_notify: Notify,
    quitting: AtomicBool,
}

impl Default for DesktopState {
    fn default() -> Self {
        Self {
            bootstrap: AsyncMutex::new(()),
            child: Mutex::new(None),
            control_file: Mutex::new(None),
            management_url: Mutex::new(None),
            owned_service: AtomicBool::new(false),
            service_terminated: AtomicBool::new(true),
            service_terminated_notify: Notify::new(),
            quitting: AtomicBool::new(false),
        }
    }
}

#[derive(Debug, Clone)]
struct ManagementAccess {
    browser_url: String,
    api_url: Url,
    token: String,
}

#[derive(Deserialize)]
struct DaemonMetadata {
    log_file: String,
    #[serde(default)]
    log_backups: usize,
}

#[tauri::command]
async fn bootstrap_pairroom(app: AppHandle) -> Result<String, String> {
    let state = app.state::<DesktopState>();
    let _guard = state.bootstrap.lock().await;

    let cached = {
        state
            .management_url
            .lock()
            .map_err(|_| "desktop state is poisoned".to_string())?
            .clone()
    };
    if let Some(cached) = cached {
        if let Ok(access) = parse_management_access(&cached) {
            if probe_management(&access).await {
                return Ok(access.browser_url);
            }
        }
        clear_cached_management(&state)?;
    }

    if let Some(explicit) = explicit_management_url().await? {
        cache_external_management(&state, &explicit)?;
        return Ok(explicit.browser_url);
    }

    if let Some(existing) = discover_daemon_management().await {
        cache_external_management(&state, &existing)?;
        return Ok(existing.browser_url);
    }

    start_owned_service(&app, &state).await
}

async fn explicit_management_url() -> Result<Option<ManagementAccess>, String> {
    let raw = match std::env::var("PAIRROOM_DESKTOP_URL") {
        Ok(value) if !value.trim().is_empty() => value,
        _ => return Ok(None),
    };
    let access = parse_management_access(&raw)
        .map_err(|error| format!("PAIRROOM_DESKTOP_URL is invalid: {error}"))?;
    if !probe_management(&access).await {
        return Err(
            "PAIRROOM_DESKTOP_URL did not authenticate a healthy numeric-loopback service"
                .to_string(),
        );
    }
    Ok(Some(access))
}

async fn discover_daemon_management() -> Option<ManagementAccess> {
    let config_root = dirs::config_dir()?.join("pairroom");
    let metadata_path = config_root.join("daemon.json");
    let data = tokio::fs::read(metadata_path).await.ok()?;
    let metadata: DaemonMetadata = serde_json::from_slice(&data).ok()?;
    if metadata.log_file.trim().is_empty() {
        return None;
    }

    let maximum_backups = metadata.log_backups.min(32);
    for index in 0..=maximum_backups {
        let path = if index == 0 {
            PathBuf::from(&metadata.log_file)
        } else {
            PathBuf::from(format!("{}.{}", metadata.log_file, index))
        };
        let content = match tokio::fs::read_to_string(path).await {
            Ok(value) => value,
            Err(_) => continue,
        };
        for line in content.lines().rev() {
            let candidate = match line.trim().strip_prefix("management:") {
                Some(value) => value.trim(),
                None => continue,
            };
            let access = match parse_management_access(candidate) {
                Ok(value) => value,
                Err(_) => continue,
            };
            if probe_management(&access).await {
                return Some(access);
            }
        }
    }
    None
}

async fn start_owned_service(app: &AppHandle, state: &DesktopState) -> Result<String, String> {
    let data_dir = app
        .path()
        .app_local_data_dir()
        .map_err(|error| format!("locate desktop data directory: {error}"))?;
    tokio::fs::create_dir_all(&data_dir)
        .await
        .map_err(|error| format!("create desktop data directory: {error}"))?;
    let control_file = data_dir.join("service.stop");
    match tokio::fs::remove_file(&control_file).await {
        Ok(()) => {}
        Err(error) if error.kind() == std::io::ErrorKind::NotFound => {}
        Err(error) => return Err(format!("clear stale desktop stop request: {error}")),
    }

    let control_text = control_file
        .to_str()
        .ok_or_else(|| "desktop control path is not valid UTF-8".to_string())?
        .to_string();
    let command = app
        .shell()
        .sidecar("pairroom-sidecar")
        .map_err(|error| format!("locate bundled PairRoom sidecar: {error}"))?
        .args([
            "service".to_string(),
            "--listen".to_string(),
            "127.0.0.1:0".to_string(),
            "--no-browser".to_string(),
            "--daemon-control-file".to_string(),
            control_text,
        ]);
    let (mut events, child) = command
        .spawn()
        .map_err(|error| format!("start bundled PairRoom service: {error}"))?;

    state
        .child
        .lock()
        .map_err(|_| "desktop child state is poisoned".to_string())?
        .replace(child);
    state
        .control_file
        .lock()
        .map_err(|_| "desktop control state is poisoned".to_string())?
        .replace(control_file);
    state.owned_service.store(true, Ordering::SeqCst);
    state.service_terminated.store(false, Ordering::SeqCst);

    let mut diagnostics = VecDeque::with_capacity(8);
    let access = match tokio::time::timeout(SERVICE_START_TIMEOUT, async {
        loop {
            match events.recv().await {
                Some(CommandEvent::Stdout(bytes)) => {
                    let line = String::from_utf8_lossy(&bytes);
                    if let Some(raw) = line.trim().strip_prefix("management:") {
                        return parse_management_access(raw.trim())
                            .map_err(|error| format!("service returned an unsafe URL: {error}"));
                    }
                }
                Some(CommandEvent::Stderr(bytes)) => {
                    push_diagnostic(&mut diagnostics, String::from_utf8_lossy(&bytes).trim());
                }
                Some(CommandEvent::Error(error)) => {
                    push_diagnostic(&mut diagnostics, error.trim());
                }
                Some(CommandEvent::Terminated(payload)) => {
                    return Err(format!(
                        "bundled PairRoom service exited before startup (code {:?}){}",
                        payload.code,
                        diagnostic_suffix(&diagnostics)
                    ));
                }
                Some(_) => {}
                None => {
                    return Err(format!(
                        "bundled PairRoom service closed its event stream before startup{}",
                        diagnostic_suffix(&diagnostics)
                    ));
                }
            }
        }
    })
    .await
    {
        Ok(Ok(value)) => value,
        Ok(Err(error)) => {
            stop_child_immediately(state);
            return Err(actionable_start_error(error));
        }
        Err(_) => {
            stop_child_immediately(state);
            return Err(actionable_start_error(format!(
                "bundled PairRoom service did not expose a Management URL within {} seconds{}",
                SERVICE_START_TIMEOUT.as_secs(),
                diagnostic_suffix(&diagnostics)
            )));
        }
    };

    if !wait_until_ready(&access).await {
        stop_child_immediately(state);
        return Err(actionable_start_error(
            "bundled PairRoom service did not authenticate after opening its listener".to_string(),
        ));
    }

    {
        let mut cached = state
            .management_url
            .lock()
            .map_err(|_| "desktop management state is poisoned".to_string())?;
        *cached = Some(access.browser_url.clone());
    }

    let monitor_app = app.clone();
    tauri::async_runtime::spawn(async move {
        while let Some(event) = events.recv().await {
            if matches!(event, CommandEvent::Terminated(_)) {
                break;
            }
        }
        let monitor_state = monitor_app.state::<DesktopState>();
        monitor_state.service_terminated.store(true, Ordering::SeqCst);
        monitor_state.owned_service.store(false, Ordering::SeqCst);
        if let Ok(mut child) = monitor_state.child.lock() {
            child.take();
        }
        if let Ok(mut cached) = monitor_state.management_url.lock() {
            cached.take();
        }
        monitor_state.service_terminated_notify.notify_waiters();
    });

    Ok(access.browser_url)
}

fn cache_external_management(
    state: &DesktopState,
    access: &ManagementAccess,
) -> Result<(), String> {
    state
        .management_url
        .lock()
        .map_err(|_| "desktop management state is poisoned".to_string())?
        .replace(access.browser_url.clone());
    state.owned_service.store(false, Ordering::SeqCst);
    state.service_terminated.store(true, Ordering::SeqCst);
    Ok(())
}

fn clear_cached_management(state: &DesktopState) -> Result<(), String> {
    state
        .management_url
        .lock()
        .map_err(|_| "desktop management state is poisoned".to_string())?
        .take();
    Ok(())
}

fn stop_child_immediately(state: &DesktopState) {
    if let Ok(mut child) = state.child.lock() {
        if let Some(child) = child.take() {
            let _ = child.kill();
        }
    }
    state.owned_service.store(false, Ordering::SeqCst);
    state.service_terminated.store(true, Ordering::SeqCst);
    state.service_terminated_notify.notify_waiters();
}

fn push_diagnostic(lines: &mut VecDeque<String>, value: &str) {
    let value = value.trim();
    if value.is_empty() {
        return;
    }
    if lines.len() == 8 {
        lines.pop_front();
    }
    lines.push_back(value.to_string());
}

fn diagnostic_suffix(lines: &VecDeque<String>) -> String {
    if lines.is_empty() {
        return String::new();
    }
    format!("\n{}", lines.iter().cloned().collect::<Vec<_>>().join("\n"))
}

fn actionable_start_error(error: String) -> String {
    format!(
        "{error}\n\nIf another foreground PairRoom service owns the default data root, set \
PAIRROOM_DESKTOP_URL to its authenticated Management URL or manage it with `pairroom daemon`. \
Use `--recover-stale-lock` only after verifying that the recorded owner process is gone."
    )
}

async fn wait_until_ready(access: &ManagementAccess) -> bool {
    let deadline = tokio::time::Instant::now() + SERVICE_READY_TIMEOUT;
    loop {
        if probe_management(access).await {
            return true;
        }
        if tokio::time::Instant::now() >= deadline {
            return false;
        }
        tokio::time::sleep(Duration::from_millis(100)).await;
    }
}

async fn probe_management(access: &ManagementAccess) -> bool {
    let client = match reqwest::Client::builder()
        .no_proxy()
        .timeout(Duration::from_secs(2))
        .build()
    {
        Ok(value) => value,
        Err(_) => return false,
    };
    match client
        .get(access.api_url.clone())
        .bearer_auth(&access.token)
        .send()
        .await
    {
        Ok(response) => response.status() == reqwest::StatusCode::OK,
        Err(_) => false,
    }
}

fn parse_management_access(raw: &str) -> Result<ManagementAccess, String> {
    let parsed = Url::parse(raw.trim()).map_err(|_| "URL could not be parsed".to_string())?;
    if parsed.scheme() != "http"
        || !parsed.username().is_empty()
        || parsed.password().is_some()
        || parsed.query().is_some()
        || (parsed.path() != "/" && !parsed.path().is_empty())
        || parsed.port().is_none()
    {
        return Err(
            "URL must be plain HTTP with an explicit numeric-loopback port, root path, no userinfo, and no query"
                .to_string(),
        );
    }
    let host = parsed
        .host_str()
        .ok_or_else(|| "URL has no host".to_string())?;
    let address =
        IpAddr::from_str(host).map_err(|_| "URL host is not a numeric IP address".to_string())?;
    if !address.is_loopback() {
        return Err("URL host is not loopback".to_string());
    }

    let fragment = parsed
        .fragment()
        .ok_or_else(|| "URL has no bootstrap token".to_string())?;
    let values: Vec<_> = url::form_urlencoded::parse(fragment.as_bytes()).collect();
    if values.len() != 1 || values[0].0 != "token" || values[0].1.trim().is_empty() {
        return Err("URL fragment must contain exactly one non-empty token".to_string());
    }

    let mut api_url = parsed.clone();
    api_url.set_fragment(None);
    api_url.set_path("/api/v1/service");
    Ok(ManagementAccess {
        browser_url: parsed.to_string(),
        api_url,
        token: values[0].1.to_string(),
    })
}

fn is_allowed_navigation(url: &Url) -> bool {
    if matches!(url.scheme(), "tauri" | "asset" | "about") {
        return true;
    }
    if url.host_str() == Some("tauri.localhost") {
        return true;
    }
    if url.scheme() != "http" {
        return false;
    }
    url.host_str()
        .and_then(|host| host.parse::<IpAddr>().ok())
        .is_some_and(|address| address.is_loopback())
}

fn build_main_window(app: &mut tauri::App) -> tauri::Result<()> {
    let app_handle = app.handle().clone();
    let window_counter = AtomicU64::new(0);
    WebviewWindowBuilder::new(app, MAIN_WINDOW, WebviewUrl::App("index.html".into()))
        .title("PairRoom")
        .inner_size(1180.0, 760.0)
        .min_inner_size(900.0, 600.0)
        .center()
        .on_navigation(is_allowed_navigation)
        .on_document_title_changed(|window, title| {
            let _ = window.set_title(&title);
        })
        .on_new_window(move |url, features| {
            if !is_allowed_navigation(&url) {
                return tauri::webview::NewWindowResponse::Deny;
            }
            let number = window_counter.fetch_add(1, Ordering::Relaxed);
            let builder = WebviewWindowBuilder::new(
                &app_handle,
                format!("room-{number}"),
                WebviewUrl::External(Url::parse("about:blank").expect("valid about:blank URL")),
            )
            .window_features(features)
            .title("PairRoom")
            .on_navigation(is_allowed_navigation)
            .on_document_title_changed(|window, title| {
                let _ = window.set_title(&title);
            });
            match builder.build() {
                Ok(window) => tauri::webview::NewWindowResponse::Create { window },
                Err(_) => tauri::webview::NewWindowResponse::Deny,
            }
        })
        .build()?;
    Ok(())
}

fn show_main_window(app: &AppHandle) {
    if let Some(window) = app.get_webview_window(MAIN_WINDOW) {
        let _ = window.unminimize();
        let _ = window.show();
        let _ = window.set_focus();
    }
}

fn setup_tray(app: &mut tauri::App) -> tauri::Result<()> {
    let show = MenuItem::with_id(app, "show", "Open PairRoom", true, None::<&str>)?;
    let quit = MenuItem::with_id(app, "quit", "Quit PairRoom", true, None::<&str>)?;
    let menu = Menu::with_items(app, &[&show, &quit])?;
    let mut builder = TrayIconBuilder::new()
        .tooltip("PairRoom")
        .menu(&menu)
        .show_menu_on_left_click(false)
        .on_menu_event(|app, event| match event.id.as_ref() {
            "show" => show_main_window(app),
            "quit" => request_quit(app.clone()),
            _ => {}
        })
        .on_tray_icon_event(|tray, event| {
            if let TrayIconEvent::Click {
                button: MouseButton::Left,
                button_state: MouseButtonState::Up,
                ..
            } = event
            {
                show_main_window(tray.app_handle());
            }
        });
    if let Some(icon) = app.default_window_icon() {
        builder = builder.icon(icon.clone());
    }
    builder.build(app)?;
    Ok(())
}

fn request_quit(app: AppHandle) {
    let state = app.state::<DesktopState>();
    if state.quitting.swap(true, Ordering::SeqCst) {
        return;
    }
    tauri::async_runtime::spawn(async move {
        shutdown_owned_service(&app).await;
        app.exit(0);
    });
}

async fn shutdown_owned_service(app: &AppHandle) {
    let state = app.state::<DesktopState>();
    if !state.owned_service.load(Ordering::SeqCst) {
        return;
    }
    let control_file = match state.control_file.lock() {
        Ok(value) => value.clone(),
        Err(_) => None,
    };
    if let Some(path) = control_file {
        let _ = tokio::fs::write(path, b"desktop quit\n").await;
    }

    let wait = async {
        loop {
            if state.service_terminated.load(Ordering::SeqCst) {
                break;
            }
            let notified = state.service_terminated_notify.notified();
            if state.service_terminated.load(Ordering::SeqCst) {
                break;
            }
            notified.await;
        }
    };
    if tokio::time::timeout(SERVICE_DRAIN_TIMEOUT, wait)
        .await
        .is_err()
    {
        stop_child_immediately(&state);
    }
}

fn main() {
    let builder = tauri::Builder::default()
        .plugin(tauri_plugin_single_instance::init(|app, _args, _cwd| {
            show_main_window(app);
        }))
        .plugin(tauri_plugin_shell::init())
        .manage(DesktopState::default())
        .invoke_handler(tauri::generate_handler![bootstrap_pairroom])
        .setup(|app| {
            build_main_window(app)?;
            setup_tray(app)?;
            Ok(())
        });

    let app = builder
        .build(tauri::generate_context!())
        .expect("error while building PairRoom desktop");

    app.run(|app, event| match event {
        RunEvent::WindowEvent {
            label,
            event: WindowEvent::CloseRequested { api, .. },
            ..
        } if label == MAIN_WINDOW && !app.state::<DesktopState>().quitting.load(Ordering::SeqCst) => {
            api.prevent_close();
            if let Some(window) = app.get_webview_window(MAIN_WINDOW) {
                let _ = window.hide();
            }
        }
        RunEvent::ExitRequested { api, code, .. }
            if code.is_none()
                && !app.state::<DesktopState>().quitting.load(Ordering::SeqCst) =>
        {
            api.prevent_exit();
        }
        _ => {}
    });
}
