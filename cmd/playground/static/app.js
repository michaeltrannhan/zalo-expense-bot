(() => {
  "use strict";

  const transcript = document.querySelector("#transcript");
  const emptyState = document.querySelector("#empty-state");
  const composer = document.querySelector("#composer");
  const input = document.querySelector("#message");
  const template = document.querySelector("#message-template");
  const queueBadge = document.querySelector("#queue-badge");
  const settingsForm = document.querySelector("#settings-form");
  const timezoneSelect = document.querySelector("#timezone");
  const currencySelect = document.querySelector("#currency");
  const sessionLabel = document.querySelector("#session-label");
  const scheduleStatus = document.querySelector("#schedule-status");
  let sessionID = newSessionID();
  let profile = null;
  let busy = false;

  function newSessionID() {
    if (globalThis.crypto?.randomUUID) return crypto.randomUUID();
    return `local-${Date.now()}-${Math.random().toString(36).slice(2, 10)}`;
  }

  function updateSessionLabel() {
    sessionLabel.textContent = sessionID.slice(0, 13);
    sessionLabel.title = sessionID;
  }

  function setBusy(next, label = "đang xử lý") {
    busy = next;
    queueBadge.textContent = next ? label : "sẵn sàng";
    queueBadge.className = next ? "queue-badge busy" : "queue-badge";
    document.querySelectorAll("button, input, select").forEach((element) => {
      element.disabled = next;
    });
  }

  function setError(message) {
    queueBadge.textContent = "có lỗi";
    queueBadge.className = "queue-badge error";
    appendMessage("system", message);
  }

  function appendMessage(role, text) {
    emptyState?.remove();
    const node = template.content.firstElementChild.cloneNode(true);
    node.classList.add(role);
    node.querySelector(".speaker").textContent = role === "user" ? "Bạn" : role === "bot" ? "Sổ Nhỏ" : "Local lab";
    node.querySelector("time").textContent = new Intl.DateTimeFormat("vi-VN", {
      hour: "2-digit", minute: "2-digit"
    }).format(new Date());
    node.querySelector("p").textContent = text;
    transcript.append(node);
    transcript.scrollTop = transcript.scrollHeight;
  }

  async function send(payload, visibleText) {
    if (busy) return;
    appendMessage("user", visibleText);
    setBusy(true, payload.fixture ? "đang đọc ảnh" : "đang trả lời");
    try {
      const response = await fetch("/api/chat", {
        method: "POST",
        headers: {"Content-Type": "application/json"},
        body: JSON.stringify({session_id: sessionID, ...payload})
      });
      const body = await response.json();
      if (!response.ok) throw new Error(body.error || `HTTP ${response.status}`);
      if (!body.replies?.length) appendMessage("system", "Tin nhắn đã được xử lý nhưng không có phản hồi mới.");
      body.replies?.forEach((reply) => appendMessage("bot", reply));
      updateProfile(body.profile || null);
    } catch (error) {
      setError(error instanceof Error ? error.message : "Không thể kết nối local playground.");
    } finally {
      setBusy(false);
      input.focus();
    }
  }

  function updateProfile(nextProfile) {
    profile = nextProfile;
    document.querySelector("#profile-status").textContent = profile ? statusLabel(profile.status) : "Chưa bắt đầu";
    document.querySelector("#profile-timezone").textContent = profile?.timezone || "Asia/Ho_Chi_Minh";
    document.querySelector("#profile-currency").textContent = profile?.default_currency || "VND";
    if (profile?.timezone && [...timezoneSelect.options].some((option) => option.value === profile.timezone)) {
      timezoneSelect.value = profile.timezone;
    }
    if (profile?.default_currency) currencySelect.value = profile.default_currency;
    const enabled = profile?.schedules?.filter((item) => item.enabled) || [];
    scheduleStatus.textContent = enabled.length
      ? enabled.map((item) => `${frequencyLabel(item.frequency)} · ${item.time}`).join(" · ")
      : "Chưa bật lịch nào";
  }

  function statusLabel(status) {
    return ({pending: "Chờ đồng ý", active: "Đang hoạt động", suspended: "Tạm dừng", deleted: "Đã xóa"})[status] || status;
  }

  function frequencyLabel(frequency) {
    return ({daily: "Ngày", weekly: "Tuần", monthly: "Tháng"})[frequency] || frequency;
  }

  composer.addEventListener("submit", (event) => {
    event.preventDefault();
    const text = input.value.trim();
    if (!text) return;
    input.value = "";
    send({text}, text);
  });

  document.addEventListener("click", (event) => {
    const command = event.target.closest("[data-message]");
    if (command) send({text: command.dataset.message}, command.dataset.message);
    const fixture = event.target.closest("[data-fixture]");
    if (fixture) send({fixture: fixture.dataset.fixture}, `📷 ${fixture.dataset.fixture}`);
  });

  settingsForm.addEventListener("submit", async (event) => {
    event.preventDefault();
    if (!profile || profile.status !== "active") {
      appendMessage("system", "Hãy bắt đầu phiên bằng /batdau trước khi đổi cài đặt.");
      return;
    }
    const commands = [];
    if (timezoneSelect.value !== profile.timezone) commands.push(`/caidat muigio ${timezoneSelect.value}`);
    if (currencySelect.value !== profile.default_currency) commands.push(`/caidat tiente ${currencySelect.value}`);
    if (!commands.length) {
      appendMessage("system", "Cài đặt đã trùng với giá trị đang dùng.");
      return;
    }
    for (const command of commands) await send({text: command}, command);
  });

  document.querySelector("#new-session").addEventListener("click", () => {
    sessionID = newSessionID();
    profile = null;
    transcript.replaceChildren();
    appendMessage("system", "Đã tạo người dùng thử mới. Gửi /batdau để bắt đầu onboarding.");
    updateSessionLabel();
    updateProfile(null);
  });

  async function checkHealth() {
    const dot = document.querySelector("#health-dot");
    const label = document.querySelector("#health-text");
    try {
      const response = await fetch("/api/health");
      if (!response.ok) throw new Error();
      dot.className = "status-dot ready";
      label.textContent = "PostgreSQL sẵn sàng";
    } catch {
      dot.className = "status-dot error";
      label.textContent = "PostgreSQL chưa sẵn sàng";
    }
  }

  updateSessionLabel();
  updateProfile(null);
  checkHealth();
})();
