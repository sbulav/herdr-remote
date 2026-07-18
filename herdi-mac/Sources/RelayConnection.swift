import Foundation
import Network
import Observation
import UserNotifications

@Observable
final class RelayConnection {
    var agents: [Agent] = []
    var isConnected = false
    var hostAddress = "ws://127.0.0.1:8375"
    var mode: ConnectionMode = .direct

    enum ConnectionMode: String, CaseIterable {
        case direct = "Direct (herdr CLI)"
        case relay = "Relay (WebSocket)"
    }

    private var task: URLSessionWebSocketTask?
    private let session = URLSession(configuration: .default)
    private var pollTimer: Timer?
    private var reconnectAttempt = 0
    private var reconnecting = false
    private let herdrPath: String
    var remotes: [String] = [] // SSH targets, e.g. ["user@host"]

    init() {
        herdrPath = ProcessInfo.processInfo.environment["HERDR_BIN"]
            ?? "/opt/homebrew/bin/herdr"
        // Load saved remotes
        if let saved = UserDefaults.standard.stringArray(forKey: "herdi_remotes") {
            remotes = saved
        }
        startDirect()
    }

    // MARK: - Direct Mode (polls herdr CLI)

    func startDirect() {
        mode = .direct
        task?.cancel(with: .normalClosure, reason: nil)
        pollTimer?.invalidate()
        pollTimer = Timer.scheduledTimer(withTimeInterval: 2.0, repeats: true) { [weak self] _ in
            self?.pollHerdr()
        }
        pollHerdr() // immediate first poll
    }

    private func pollHerdr() {
        DispatchQueue.global(qos: .utility).async { [self] in
            // Local
            var allAgents = parseAgents(from: runHerdr("pane", "list"), host: "local")

            // Remotes via SSH
            for remote in remotes {
                let result = runSSH(remote, "herdr", "pane", "list")
                allAgents += parseAgents(from: result, host: remote)
            }

            DispatchQueue.main.async { [self] in
                isConnected = true
                var seen = Set<String>()
                for a in allAgents {
                    seen.insert(a.id)
                    if let existing = agents.first(where: { $0.id == a.id }) {
                        if existing.status != a.status {
                            if a.status == .blocked && existing.status != .blocked {
                                readPaneForBlocked(existing, remote: a.host == "local" ? nil : a.host)
                            }
                            existing.status = a.status
                        }
                        if existing.project != a.project { existing.project = a.project }
                        if existing.host != a.host { existing.host = a.host }
                    } else {
                        let agent = Agent(id: a.id, name: a.name, status: a.status, project: a.project, cwd: a.cwd, host: a.host)
                        agents.append(agent)
                        if a.status == .blocked { readPaneForBlocked(agent, remote: a.host == "local" ? nil : a.host) }
                    }
                }
                agents.removeAll { !seen.contains($0.id) }
            }
        }
    }

    private struct ParsedAgent {
        let id: String, name: String, status: AgentStatus, project: String, cwd: String, host: String
    }

    private func parseAgents(from output: String, host: String) -> [ParsedAgent] {
        guard let data = output.data(using: .utf8),
              let json = try? JSONSerialization.jsonObject(with: data) as? [String: Any],
              let resultObj = json["result"] as? [String: Any],
              let panes = resultObj["panes"] as? [[String: Any]] else { return [] }

        return panes.compactMap { p in
            guard let agent = p["agent"] as? String, !agent.isEmpty else { return nil }
            let paneId = (host == "local" ? "" : "\(host):") + (p["pane_id"] as? String ?? "")
            let status = AgentStatus(rawValue: p["agent_status"] as? String ?? "unknown") ?? .unknown
            let cwd = p["cwd"] as? String ?? ""
            return ParsedAgent(id: paneId, name: agent, status: status, project: (cwd as NSString).lastPathComponent, cwd: cwd, host: host)
        }
    }

    private func runSSH(_ remote: String, _ args: String...) -> String {
        runSSH(remote, arguments: Array(args))
    }

    private func runSSH(_ remote: String, arguments args: [String]) -> String {
        let process = Process()
        let password = KeychainHelper.getPassword(for: remote)

        if let password, FileManager.default.fileExists(atPath: "/opt/homebrew/bin/sshpass") {
            // Use sshpass for password auth
            process.executableURL = URL(fileURLWithPath: "/opt/homebrew/bin/sshpass")
            process.arguments = ["-p", password, "ssh", "-o", "ConnectTimeout=5", "-o", "StrictHostKeyChecking=no", remote] + args
        } else {
            process.executableURL = URL(fileURLWithPath: "/usr/bin/ssh")
            process.arguments = ["-o", "ConnectTimeout=5", "-o", "BatchMode=yes", remote] + args
        }

        let pipe = Pipe()
        process.standardOutput = pipe
        process.standardError = FileHandle.nullDevice
        do {
            try process.run()
            process.waitUntilExit()
            guard process.terminationStatus == 0 else { return "" }
            return String(data: pipe.fileHandleForReading.readDataToEndOfFile(), encoding: .utf8) ?? ""
        } catch { return "" }
    }

    func addRemote(_ remote: String, password: String? = nil) {
        guard !remote.isEmpty, !remotes.contains(remote) else { return }
        remotes.append(remote)
        UserDefaults.standard.set(remotes, forKey: "herdi_remotes")
        if let password, !password.isEmpty {
            KeychainHelper.setPassword(password, for: remote)
        }
    }

    func removeRemote(_ remote: String) {
        remotes.removeAll { $0 == remote }
        UserDefaults.standard.set(remotes, forKey: "herdi_remotes")
        KeychainHelper.deletePassword(for: remote)
    }

    private func readPaneForBlocked(_ agent: Agent, remote: String? = nil) {
        // Extract the real pane_id (strip host prefix if present)
        let paneId = agent.id.contains(":") && remote != nil
            ? String(agent.id.drop(while: { $0 != ":" }).dropFirst())
            : agent.id

        DispatchQueue.global(qos: .utility).async { [self] in
            let raw: String
            if let remote {
                raw = runSSH(remote, "herdr", "pane", "read", paneId, "--lines", "20", "--source", "recent")
            } else {
                raw = runHerdr("pane", "read", paneId, "--lines", "20", "--source", "recent")
            }
            let lines = raw.components(separatedBy: .newlines)
                .filter { !$0.trimmingCharacters(in: .whitespaces).isEmpty }
                .suffix(6)
            let content = lines.joined(separator: "\n")
            let options = detectOptions(content)

            DispatchQueue.main.async {
                agent.prompt = String(content.prefix(500))
                agent.options = options
                self.sendNotification(agent: agent.name, project: agent.project)
            }
        }
    }

    private func detectOptions(_ text: String) -> [String] {
        let lower = text.lowercased()
        if lower.contains("yes, single permission") {
            return ["yes, single permission", "trust, always allow", "no (tab to edit)"]
        }
        if lower.contains("approve all pending") || lower.contains("pending from subagents") {
            return ["approve all pending", "configure individually", "exit (cancel subagents)"]
        }
        if lower.contains("permission required")
            || (lower.contains("allow once") && lower.contains("allow always") && lower.contains("reject")) {
            return ["Allow once", "Allow always", "Reject"]
        }
        // Claude Code numbered menus: "❯ 1. Yes" / "  2. No"
        let pattern = #"(?:^|\n)[ \t]*[❯>]?[ \t]*(\d+)\.\s+(\S[^\n]*)"#
        if let re = try? NSRegularExpression(pattern: pattern) {
            let ns = text as NSString
            let matches = re.matches(in: text, range: NSRange(location: 0, length: ns.length))
            var seen: [String: String] = [:]
            for m in matches {
                let num = ns.substring(with: m.range(at: 1))
                let label = ns.substring(with: m.range(at: 2)).trimmingCharacters(in: .whitespaces)
                if seen[num] == nil { seen[num] = "\(num). \(label)" }
            }
            let opts = seen.keys.sorted { Int($0) ?? 0 < Int($1) ?? 0 }.compactMap { seen[$0] }
            if opts.count >= 2 { return opts }
        }
        if lower.contains("do you want to proceed")
            || lower.contains("do you want to allow")
            || lower.contains("ask rule")
            || lower.contains("/permissions to let auto mode decide") {
            return ["1. Yes", "2. No"]
        }
        if lower.contains("[y/n]") || lower.contains("yes (y)") || lower.contains("proceed (y)") {
            return ["y", "n"]
        }
        return ["1. Yes", "2. No"]
    }

    private func runHerdr(_ args: String...) -> String {
        runHerdr(arguments: Array(args))
    }

    private func runHerdr(arguments args: [String]) -> String {
        let process = Process()
        process.executableURL = URL(fileURLWithPath: herdrPath)
        process.arguments = args
        let pipe = Pipe()
        process.standardOutput = pipe
        process.standardError = FileHandle.nullDevice
        do {
            try process.run()
            process.waitUntilExit()
            return String(data: pipe.fileHandleForReading.readDataToEndOfFile(), encoding: .utf8) ?? ""
        } catch {
            return ""
        }
    }

    // MARK: - Relay Mode (WebSocket)

    func connectRelay(to urlString: String) {
        guard let url = URL(string: urlString) else { return }
        mode = .relay
        hostAddress = urlString
        pollTimer?.invalidate()
        pollTimer = nil
        reconnecting = false
        task?.cancel(with: .normalClosure, reason: nil)
        task = session.webSocketTask(with: url)
        task?.resume()
        reconnectAttempt = 0
        listen()
    }

    func disconnect() {
        task?.cancel(with: .normalClosure, reason: nil)
        pollTimer?.invalidate()
        isConnected = false
    }

    private func respondKeystrokes(_ text: String) -> String {
        // Numbered menu labels ("1. Yes") must send just the digit.
        // OpenCode menus are handled by the relay (keys); direct mode maps here.
        let lower = text.lowercased()
        if let re = try? NSRegularExpression(pattern: #"^(\d+)\.\s+"#),
           let m = re.firstMatch(in: text, range: NSRange(location: 0, length: (text as NSString).length)) {
            return (text as NSString).substring(with: m.range(at: 1))
        }
        if lower == "y" || lower == "yes" { return "y" }
        if lower == "n" || lower == "no" { return "n" }
        return text
    }

    private func respondDirectKeys(_ text: String) -> [String]? {
        switch text.lowercased() {
        case "allow once": return ["Enter"]
        case "allow always", "always allow": return ["Right", "Enter", "Enter"]
        case "reject": return ["Escape"]
        default: return nil
        }
    }

    func send(response: ResponseMessage) {
        if mode == .direct {
            DispatchQueue.global(qos: .userInitiated).async { [self] in
                let paneId = response.pane_id
                let host: String? = {
                    if let agent = agents.first(where: { $0.id == paneId }), agent.host != "local" {
                        return agent.host
                    }
                    return nil
                }()
                let realId = host.map { _ in String(paneId.drop(while: { $0 != ":" }).dropFirst()) } ?? paneId
                if let keys = respondDirectKeys(response.text) {
                    var args = ["pane", "send-keys", realId]
                    args.append(contentsOf: keys)
                    if let host {
                        _ = runSSH(host, arguments: ["herdr"] + args)
                    } else {
                        _ = runHerdr(arguments: args)
                    }
                } else {
                    let text = respondKeystrokes(response.text)
                    if let host {
                        _ = runSSH(host, "herdr", "pane", "send-text", realId, text + "\n")
                    } else {
                        _ = runHerdr("pane", "send-text", paneId, text + "\n")
                    }
                }
            }
        } else {
            // Relay maps labels (incl. OpenCode keys) itself.
            guard let data = try? JSONEncoder().encode(response) else { return }
            task?.send(.string(String(data: data, encoding: .utf8)!)) { _ in }
        }
    }

    func focusPane(_ paneId: String) {
        let host = agents.first(where: { $0.id == paneId })?.host
        let remote = host == nil || host == "local" ? nil : host
        let realId = remote.map { _ in
            String(paneId.drop(while: { $0 != ":" }).dropFirst())
        } ?? paneId
        DispatchQueue.global(qos: .userInitiated).async { [self] in
            if let remote {
                _ = runSSH(remote, "herdr", "pane", "focus", realId)
            } else {
                _ = runHerdr("pane", "focus", realId)
            }
        }
    }

    func interruptPane(_ paneId: String) {
        let host = agents.first(where: { $0.id == paneId })?.host
        let remote = host == nil || host == "local" ? nil : host
        let realId = remote.map { _ in
            String(paneId.drop(while: { $0 != ":" }).dropFirst())
        } ?? paneId
        DispatchQueue.global(qos: .userInitiated).async { [self] in
            if let remote {
                _ = runSSH(remote, "herdr", "pane", "send-keys", realId, "Ctrl+c")
            } else {
                _ = runHerdr("pane", "send-keys", realId, "Ctrl+c")
            }
        }
    }

    private func listen() {
        task?.receive { [weak self] result in
            guard let self else { return }
            switch result {
            case .success(let message):
                DispatchQueue.main.async { if !self.isConnected { self.isConnected = true } }
                switch message {
                case .string(let text): self.handleWS(text)
                case .data(let data): self.handleWS(String(data: data, encoding: .utf8) ?? "")
                @unknown default: break
                }
                self.listen()
            case .failure:
                DispatchQueue.main.async {
                    self.isConnected = false
                    self.scheduleReconnect()
                }
            }
        }
    }

    private func scheduleReconnect() {
        guard !reconnecting, mode == .relay else { return }
        reconnecting = true
        reconnectAttempt += 1
        let delay = min(Double(1 << min(reconnectAttempt, 5)), 30.0)
        DispatchQueue.main.asyncAfter(deadline: .now() + delay) { [weak self] in
            guard let self, !self.isConnected else { return }
            self.reconnecting = false
            self.connectRelay(to: self.hostAddress)
        }
    }

    private func handleWS(_ text: String) {
        guard let data = text.data(using: .utf8),
              let msg = try? JSONDecoder().decode(AgentMessage.self, from: data) else { return }
        DispatchQueue.main.async { [self] in
            switch msg.type {
            case "agents":
                guard let list = msg.agents else { return }
                var seen = Set<String>()
                for a in list {
                    seen.insert(a.pane_id)
                    if let existing = agents.first(where: { $0.id == a.pane_id }) {
                        let s = AgentStatus(rawValue: a.status) ?? .unknown
                        if existing.status != s { existing.status = s }
                        if existing.project != a.project { existing.project = a.project }
                        existing.host = a.host ?? "local"
                    } else {
                        agents.append(Agent(
                            id: a.pane_id, name: a.agent,
                            status: AgentStatus(rawValue: a.status) ?? .unknown,
                            project: a.project, cwd: a.cwd, host: a.host ?? "local"
                        ))
                    }
                }
                agents.removeAll { !seen.contains($0.id) }
            case "blocked":
                if let pid = msg.pane_id, let agent = agents.first(where: { $0.id == pid }) {
                    agent.prompt = msg.prompt
                    agent.options = msg.options
                    agent.status = .blocked
                    sendNotification(agent: agent.name, project: agent.project)
                }
            default: break
            }
        }
    }

    private func sendNotification(agent: String, project: String) {
        let center = UNUserNotificationCenter.current()
        let content = UNMutableNotificationContent()
        content.title = "Agent Blocked"
        content.body = "\(agent) needs input in \(project)"
        content.sound = .default
        center.add(UNNotificationRequest(identifier: UUID().uuidString, content: content, trigger: nil))
    }
}
