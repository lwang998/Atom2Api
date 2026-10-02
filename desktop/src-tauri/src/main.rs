// Atom2Api desktop shell: starts the bundled atom2api server as a sidecar
// process, waits for its HTTP port, then opens the embedded console in a
// native window. Config and data live next to the desktop executable.
#![cfg_attr(not(debug_assertions), windows_subsystem = "windows")]

use std::net::TcpStream;
use std::time::{Duration, Instant};

use tauri::{Manager, WebviewUrl, WebviewWindowBuilder};
use tauri_plugin_shell::process::{CommandChild, CommandEvent};
use tauri_plugin_shell::ShellExt;

const SERVER_ADDR: &str = "127.0.0.1:8080";
const CONSOLE_URL: &str = "http://127.0.0.1:8080";

struct ServerChild(std::sync::Mutex<Option<CommandChild>>);

fn main() {
    let app = tauri::Builder::default()
        .plugin(tauri_plugin_shell::init())
        .manage(ServerChild(std::sync::Mutex::new(None)))
        .setup(|app| {
            let handle = app.handle().clone();
            let workdir = std::env::current_exe()?
                .parent()
                .map(|p| p.to_path_buf())
                .unwrap_or_default();
            // The sidecar ships as atom2api-server.exe; naming it differently
            // from the shell (Atom2Api.exe) avoids the NTFS case-insensitive
            // filename collision between Atom2Api.exe and atom2api.exe.
            let command = handle.shell().sidecar("atom2api-server")?.current_dir(workdir);
            let (mut rx, child) = command.spawn()?;
            *handle.state::<ServerChild>().0.lock().unwrap() = Some(child);
            tauri::async_runtime::spawn(async move {
                while let Some(event) = rx.recv().await {
                    if let CommandEvent::Terminated(_) = event {
                        break;
                    }
                }
            });

            // Give the server up to 30 s to bind its port before showing the
            // console; an already-running instance on the same port also
            // satisfies this check.
            let deadline = Instant::now() + Duration::from_secs(30);
            while Instant::now() < deadline {
                if TcpStream::connect(SERVER_ADDR).is_ok() {
                    break;
                }
                std::thread::sleep(Duration::from_millis(250));
            }

            WebviewWindowBuilder::new(
                app,
                "main",
                WebviewUrl::External(CONSOLE_URL.parse()?),
            )
            .title("Atom2Api")
            .inner_size(1180.0, 780.0)
            .min_inner_size(900.0, 600.0)
            .build()?;
            Ok(())
        })
        .build(tauri::generate_context!())
        .expect("error while building Atom2Api desktop shell");

    // Terminate the sidecar server when the app exits, including a normal
    // window close (a force kill bypasses this, as with any process).
    app.run(|app_handle, event| {
        if let tauri::RunEvent::Exit = event {
            if let Some(child) = app_handle.state::<ServerChild>().0.lock().unwrap().take() {
                let _ = child.kill();
            }
        }
    });
}
