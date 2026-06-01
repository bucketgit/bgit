const reviewDraftComments = [];
let restoringReviewDraft = false;
const prScrollTargetKey = 'bgit.prScrollTarget';
let currentWhoami = window.BGIT_WHOAMI || null;

document.addEventListener('click', function (event) {
  const contextButton = event.target.closest('[data-diff-context]');
  if (contextButton) {
    event.preventDefault();
    revealDiffContext(contextButton);
    return;
  }

  const fileResult = event.target.closest('[data-file-search-result]');
  if (fileResult) {
    event.preventDefault();
    navigateToLocalURL(fileResult.getAttribute('data-file-search-result'));
    return;
  }

  const prItem = event.target.closest('[data-pr-href]');
  if (prItem && !event.target.closest('a, button, input, textarea, select, label')) {
    event.preventDefault();
    navigateToLocalURL(prItem.getAttribute('data-pr-href'));
    return;
  }

  const disabledCapabilityLink = event.target.closest('a.is-capability-disabled');
  if (disabledCapabilityLink) {
    event.preventDefault();
    setSyncStatus(disabledCapabilityLink.getAttribute('title') || 'Your current broker role does not allow this action.', 'is-stale');
    return;
  }

  const commitRow = event.target.closest('[data-commit-href]');
  if (commitRow && !event.target.closest('a, button, input, textarea, select, label, .commit-inline-detail')) {
    event.preventDefault();
    navigateToLocalURL(commitRow.getAttribute('data-commit-href'));
    return;
  }

  const codeToggle = event.target.closest('[data-code-menu-toggle]');
  if (codeToggle) {
    const menu = codeToggle.closest('.code-menu');
    const popover = menu ? menu.querySelector('[data-code-menu]') : null;
    if (!popover) return;
    const open = popover.hidden;
    closeCodeMenus(menu);
    popover.hidden = !open;
    codeToggle.setAttribute('aria-expanded', open ? 'true' : 'false');
    return;
  }

  if (!event.target.closest('.code-menu')) {
    closeCodeMenus(null);
  }
  if (!event.target.closest('.file-search')) {
    closeFileSearchResults();
  }

  const refCount = event.target.closest('[data-focus-ref-selector]');
  if (refCount) {
    event.preventDefault();
    const selector = document.querySelector('[data-ref-selector]');
    if (selector) {
      selector.focus();
      if (typeof selector.showPicker === 'function') {
        try { selector.showPicker(); } catch (_) {}
      }
    }
    return;
  }

  const refresh = event.target.closest('[data-remote-refresh]');
  if (refresh) {
    refreshRemoteState({manual: true, refreshPullRequests: true});
    return;
  }

  const syncBadge = event.target.closest('[data-remote-sync-badge]');
  if (syncBadge && currentWebState && Number(currentWebState.behind || 0) > 0) {
    handleWebAction('pull');
    return;
  }

  const diffAction = event.target.closest('[data-web-diff]');
  if (diffAction) {
    event.preventDefault();
    showInlineDiff(diffAction);
    return;
  }

  const webAction = event.target.closest('[data-web-action]');
  if (webAction) {
    event.preventDefault();
    if (!hasCapability(webAction.getAttribute('data-capability') || '')) {
      setSyncStatus('Your current broker role does not allow this action.', 'is-stale');
      return;
    }
    handleWebAction(webAction.getAttribute('data-web-action'), webAction);
    return;
  }

  const prAction = event.target.closest('[data-pr-action]');
  if (prAction) {
    event.preventDefault();
    if (!hasCapability(prAction.getAttribute('data-capability') || '')) {
      setSyncStatus('Your current broker role does not allow this action.', 'is-stale');
      return;
    }
    handlePullRequestAction(prAction);
    return;
  }

  const prReply = event.target.closest('[data-pr-reply]');
  if (prReply) {
    event.preventDefault();
    showPullRequestReplyEditor(prReply);
    return;
  }

  const reviewCommentButton = event.target.closest('[data-review-comment-line], [data-review-comment-file]');
  if (reviewCommentButton) {
    event.preventDefault();
    showReviewDraftEditor(reviewCommentButton);
    return;
  }

  const draftOK = event.target.closest('[data-draft-ok]');
  if (draftOK) {
    event.preventDefault();
    submitInlineDraft(draftOK);
    return;
  }

  const draftCancel = event.target.closest('[data-draft-cancel]');
  if (draftCancel) {
    event.preventDefault();
    const editor = draftCancel.closest('[data-draft-editor]');
    if (editor) editor.remove();
    saveReviewDraftState();
    return;
  }

  const reviewSubmit = event.target.closest('[data-pr-review-action]');
  if (reviewSubmit) {
    event.preventDefault();
    if (!hasCapability(reviewSubmit.getAttribute('data-capability') || '')) {
      setSyncStatus('Your current broker role does not allow this action.', 'is-stale');
      return;
    }
    submitReviewDraft(reviewSubmit);
    return;
  }

  const reviewCancel = event.target.closest('[data-review-cancel]');
  if (reviewCancel) {
    clearStoredReviewDraft(currentReviewID());
    return;
  }

  const settingsAction = event.target.closest('[data-settings-action]');
  if (settingsAction) {
    event.preventDefault();
    handleSettingsAction(settingsAction);
    return;
  }

  const issueAction = event.target.closest('[data-issue-action]');
  if (issueAction) {
    event.preventDefault();
    handleIssueAction(issueAction);
    return;
  }

  const boardAction = event.target.closest('[data-board-action]');
  if (boardAction) {
    event.preventDefault();
    handleBoardAction(boardAction);
    return;
  }

  const boardOnlyMe = event.target.closest('[data-board-only-me]');
  if (boardOnlyMe) {
    event.preventDefault();
    toggleBoardOnlyMe(boardOnlyMe);
    return;
  }

  const userMenuToggle = event.target.closest('[data-user-menu-toggle]');
  if (userMenuToggle) {
    event.preventDefault();
    toggleUserMenu(userMenuToggle);
    return;
  }
  if (!event.target.closest('.user-menu')) {
    closeUserMenus();
  }

  const reassignToggle = event.target.closest('[data-board-reassign-toggle]');
  if (reassignToggle) {
    event.preventDefault();
    const actions = reassignToggle.closest('.story-actions, .story-detail-actions');
    const select = actions ? actions.querySelector('[data-board-reassign]') : null;
    const lane = actions ? actions.querySelector('[data-board-lane]') : null;
    if (select) {
      const opening = select.hidden;
      select.hidden = !opening;
      if (opening && lane) lane.hidden = true;
      actions?.classList.toggle('is-reassigning', opening);
      actions?.classList.remove('is-moving');
      actions?.closest('[data-story-id]')?.classList.toggle('is-menu-open', opening);
      if (opening) select.focus();
    }
    return;
  }

  const moveToggle = event.target.closest('[data-board-move-toggle]');
  if (moveToggle) {
    event.preventDefault();
    const actions = moveToggle.closest('.story-actions, .story-detail-actions');
    const lane = actions ? actions.querySelector('[data-board-lane]') : null;
    const reassign = actions ? actions.querySelector('[data-board-reassign]') : null;
    if (lane) {
      const opening = lane.hidden;
      lane.hidden = !opening;
      if (opening && reassign) reassign.hidden = true;
      actions?.classList.toggle('is-moving', opening);
      actions?.classList.remove('is-reassigning');
      actions?.closest('[data-story-id]')?.classList.toggle('is-menu-open', opening);
      if (opening) lane.focus();
    }
    return;
  }

  const cloneTab = event.target.closest('[data-clone-tab]');
  if (cloneTab) {
    const panel = cloneTab.closest('.clone-panel');
    const target = cloneTab.getAttribute('data-clone-tab');
    if (!panel || !target) return;
    for (const tab of panel.querySelectorAll('[data-clone-tab]')) {
      const active = tab === cloneTab;
      tab.classList.toggle('active', active);
      tab.setAttribute('aria-selected', active ? 'true' : 'false');
    }
    for (const pane of panel.querySelectorAll('[data-clone-pane]')) {
      pane.hidden = pane.getAttribute('data-clone-pane') !== target;
    }
    return;
  }

  const button = event.target.closest('[data-copy-target]');
  if (!button) return;
  const target = document.getElementById(button.getAttribute('data-copy-target'));
  if (!target) return;
  const value = target.value !== undefined ? target.value : target.textContent;
  navigator.clipboard.writeText(value).then(function () {
    if (button.hasAttribute('data-copy-icon')) {
      const oldTitle = button.getAttribute('title') || 'Copy';
      const oldLabel = button.getAttribute('aria-label') || 'Copy';
      button.classList.add('is-copied');
      button.setAttribute('title', 'Copied');
      button.setAttribute('aria-label', 'Copied');
      window.setTimeout(function () {
        button.classList.remove('is-copied');
        button.setAttribute('title', oldTitle);
        button.setAttribute('aria-label', oldLabel);
      }, 1200);
      return;
    }
    const old = button.textContent;
    button.textContent = 'Copied';
    window.setTimeout(function () { button.textContent = old; }, 1200);
  });
});

document.addEventListener('change', function (event) {
  const lane = event.target.closest('[data-board-lane]');
  if (lane) {
    handleBoardLaneChange(lane);
    return;
  }
  const reassign = event.target.closest('[data-board-reassign]');
  if (reassign) {
    handleBoardReassignChange(reassign);
    return;
  }
  const select = event.target.closest('[data-ref-selector]');
  if (!select) return;
  const url = new URL(window.location.href);
  url.searchParams.set('ref', select.value);
  window.location.href = url.toString();
});

document.addEventListener('keydown', function (event) {
  if (event.key !== 'Escape') return;
  const reassign = event.target.closest('[data-board-reassign]');
  if (reassign && !reassign.hidden) {
    event.preventDefault();
    cancelBoardReassign(reassign);
    return;
  }
  const lane = event.target.closest('[data-board-lane]');
  if (lane && !lane.hidden) {
    event.preventDefault();
    cancelBoardMove(lane);
  }
});

document.addEventListener('dragstart', function (event) {
  const card = event.target.closest('[data-story-id]');
  if (!card || card.classList.contains('is-committing') || !hasCapability('push')) {
    event.preventDefault();
    return;
  }
  event.dataTransfer.effectAllowed = 'move';
  event.dataTransfer.setData('text/plain', card.getAttribute('data-story-id') || '');
  event.dataTransfer.setData('application/x-bgit-story-lane', card.getAttribute('data-story-lane') || '');
  card.classList.add('is-dragging');
});

document.addEventListener('dragend', function (event) {
  const card = event.target.closest('[data-story-id]');
  if (card) card.classList.remove('is-dragging');
  clearBoardDropTargets();
});

document.addEventListener('dragover', function (event) {
  const lane = event.target.closest('[data-board-drop-lane]');
  if (!lane || !hasCapability('push')) return;
  event.preventDefault();
  event.dataTransfer.dropEffect = 'move';
  lane.classList.add('is-drop-target');
});

document.addEventListener('dragleave', function (event) {
  const lane = event.target.closest('[data-board-drop-lane]');
  if (!lane || lane.contains(event.relatedTarget)) return;
  lane.classList.remove('is-drop-target');
});

document.addEventListener('drop', function (event) {
  const lane = event.target.closest('[data-board-drop-lane]');
  if (!lane || !hasCapability('push')) return;
  event.preventDefault();
  clearBoardDropTargets();
  const id = Number(event.dataTransfer.getData('text/plain') || 0);
  const fromLane = event.dataTransfer.getData('application/x-bgit-story-lane') || '';
  const toLane = lane.getAttribute('data-board-drop-lane') || '';
  if (!id || !toLane) return;
  const afterID = boardDropAfterID(lane, event.clientY, id);
  if (fromLane === toLane) reorderBoardStory(id, toLane, afterID);
  else moveBoardStory(id, toLane, afterID);
});

document.addEventListener('submit', function (event) {
  const form = event.target.closest('[data-settings-form]');
  if (form) {
    event.preventDefault();
    handleSettingsForm(form);
    return;
  }
  const issueForm = event.target.closest('[data-issue-form]');
  if (issueForm) {
    event.preventDefault();
    handleIssueForm(issueForm);
    return;
  }
  const boardForm = event.target.closest('[data-board-form]');
  if (boardForm) {
    event.preventDefault();
    handleBoardForm(boardForm);
    return;
  }
  const ciForm = event.target.closest('[data-ci-form]');
  if (ciForm) {
    event.preventDefault();
    handleCIForm(ciForm);
    return;
  }
  const prCreateForm = event.target.closest('[data-pr-create-form]');
  if (prCreateForm) {
    event.preventDefault();
    handlePullRequestCreate(prCreateForm);
  }
});

function navigateToLocalURL(value) {
  if (!value) return;
  let url;
  try {
    url = new URL(value, window.location.origin);
  } catch (_) {
    return;
  }
  if (url.origin !== window.location.origin) return;
  if (url.protocol !== 'http:' && url.protocol !== 'https:') return;
  window.location.assign(url.pathname + url.search + url.hash);
}

document.addEventListener('input', function (event) {
  const input = event.target.closest('[data-file-search]');
  if (!input) return;
  renderFileSearchResults(input);
});

document.addEventListener('input', function (event) {
  if (event.target.closest('[data-pr-review-note], [data-draft-editor] [data-draft-text]')) {
    saveReviewDraftState();
  }
});

document.addEventListener('keydown', function (event) {
  const input = event.target.closest('[data-file-search]');
  if (!input || event.key !== 'Enter') return;
  const match = findIndexedFile(input.value);
  if (!match) return;
  event.preventDefault();
  navigateToLocalURL(match.url);
});

document.addEventListener('DOMContentLoaded', function () {
  setupThemeToggle();
  setupUserSettingsPage();
  setupPullRequestCreatePage();
  setupReviewDiff();
  restorePullRequestScrollTarget();
  setWhoamiState(currentWhoami);
  restoreSettingsStatus();
  connectBgitEvents();
  refreshWhoamiState();
  hydrateRefs();
  refreshSettingsSections();
  refreshCIRuns();
  refreshRemoteState({refreshPullRequests: false});
  window.setInterval(function () { refreshRemoteState({refreshPullRequests: true}); }, 30000);
});

function setupReviewDiff() {
  const review = document.querySelector('[data-pr-review-diff]');
  if (!review) return;
  const existing = readReviewComments();
  for (const file of review.querySelectorAll('[data-review-file]')) {
    const path = file.getAttribute('data-review-file') || '';
    const fileComments = existing.filter((comment) => comment.file === path && comment.kind === 'file');
    fileComments.forEach((comment) => { comment._matched = true; });
    const fileButton = file.querySelector('[data-review-comment-file]');
    if (fileButton && fileComments.length) fileButton.innerHTML = '💬<span class="comment-count">' + fileComments.length + '</span>';
    if (fileComments.length) {
      file.querySelector('.diff-header')?.insertAdjacentHTML('afterend', reviewThreadHTML(fileComments));
    }
    for (const row of file.querySelectorAll('.visual-diff-row')) {
      const newCell = row.querySelector('pre[data-new-line]');
      const line = newCell ? newCell.getAttribute('data-new-line') : '';
      if (!newCell || !line) continue;
      newCell.classList.add('review-comment-target');
      const rowComments = existing.filter((comment) => comment.file === path && comment.kind === 'line' && Number(comment.line || 0) === Number(line));
      rowComments.forEach((comment) => { comment._matched = true; });
      newCell.insertAdjacentHTML('beforeend', '<button type="button" class="review-comment-button line-comment" data-review-comment-line data-file="' + escapeHTML(path) + '" data-line="' + escapeHTML(line) + '" aria-label="Comment on line" title="Comment on line">💬' + (rowComments.length ? '<span class="comment-count">' + rowComments.length + '</span>' : '') + '</button>');
      if (rowComments.length) row.insertAdjacentHTML('afterend', '<div class="visual-diff-row review-thread-row"><div></div><pre></pre><div></div><pre>' + reviewThreadHTML(rowComments) + '</pre></div>');
    }
    const orphaned = existing.filter((comment) => comment.file === path && comment.kind === 'line' && !comment._matched);
    if (orphaned.length) {
      file.querySelector('.visual-diff-grid')?.insertAdjacentHTML('beforeend', '<div class="visual-diff-row review-thread-row"><div></div><pre></pre><div></div><pre><div class="muted">Outdated comments</div>' + reviewThreadHTML(orphaned) + '</pre></div>');
    }
  }
  restoreReviewDraftState();
}

function readReviewComments() {
  const node = document.getElementById('pr-review-comments');
  if (!node) return [];
  try {
    const comments = JSON.parse(node.textContent || '[]');
    return Array.isArray(comments) ? comments : [];
  } catch (_) {
    return [];
  }
}

function reviewThreadHTML(comments) {
  return '<div class="review-thread">' + comments.map(function (comment) {
    return '<div class="review-thread-comment"><div class="pr-reply-meta"><strong>' + escapeHTML(comment.user || 'unknown') + '</strong> commented' + (comment.at ? ' <span>' + escapeHTML(comment.at) + '</span>' : '') + '</div><div>' + escapeHTML(comment.body || '') + '</div></div>';
  }).join('') + '</div>';
}

function closeCodeMenus(except) {
  for (const menu of document.querySelectorAll('.code-menu')) {
    if (except && menu === except) continue;
    const popover = menu.querySelector('[data-code-menu]');
    const toggle = menu.querySelector('[data-code-menu-toggle]');
    if (popover) popover.hidden = true;
    if (toggle) toggle.setAttribute('aria-expanded', 'false');
  }
}

function indexedFiles() {
  if (indexedFiles.cache) return indexedFiles.cache;
  const el = document.getElementById('bgit-file-index');
  if (!el) {
    indexedFiles.cache = [];
    return indexedFiles.cache;
  }
  try {
    const parsed = JSON.parse(el.textContent || '[]');
    indexedFiles.cache = Array.isArray(parsed) ? parsed : [];
  } catch (_) {
    indexedFiles.cache = [];
  }
  return indexedFiles.cache;
}

function findIndexedFile(value) {
  const query = String(value || '').trim().toLowerCase();
  if (!query) return null;
  const files = rankedIndexedFiles(query);
  return files[0] || null;
}

function rankedIndexedFiles(query) {
  query = String(query || '').trim().toLowerCase();
  if (!query) return [];
  const exact = [];
  const prefix = [];
  const segmentPrefix = [];
  for (const file of indexedFiles()) {
    const path = String(file.path || '');
    const lower = path.toLowerCase();
    if (lower === query) exact.push(file);
    else if (lower.startsWith(query)) prefix.push(file);
    else if (lower.split('/').some(function (part) { return part.startsWith(query); })) segmentPrefix.push(file);
  }
  return exact.concat(prefix, segmentPrefix);
}

function renderFileSearchResults(input) {
  const results = document.querySelector('[data-file-search-results]');
  if (!results) return;
  const matches = rankedIndexedFiles(input.value).slice(0, 12);
  if (matches.length === 0) {
    results.hidden = true;
    input.setAttribute('aria-expanded', 'false');
    results.innerHTML = '';
    return;
  }
  results.innerHTML = matches.map(function (file, index) {
    const icon = file.kind === 'dir' ? '▣' : '▯';
    return '<a href="' + escapeHTML(file.url || '#') + '" data-file-search-result="' + escapeHTML(file.url || '#') + '"' + (index === 0 ? ' class="active"' : '') + '><span class="file-search-icon" aria-hidden="true">' + icon + '</span><span>' + escapeHTML(file.path || '') + '</span></a>';
  }).join('');
  results.hidden = false;
  input.setAttribute('aria-expanded', 'true');
}

function closeFileSearchResults() {
  const results = document.querySelector('[data-file-search-results]');
  const input = document.querySelector('[data-file-search]');
  if (results) {
    results.hidden = true;
    results.innerHTML = '';
  }
  if (input) input.setAttribute('aria-expanded', 'false');
}

let remoteRefreshInFlight = false;
let remoteSyncInitialized = false;
let currentWebState = null;

function setupThemeToggle() {
  const button = document.querySelector('[data-theme-toggle]');
  if (!button) return;
  const storageKey = 'bgit.theme';
  const media = window.matchMedia ? window.matchMedia('(prefers-color-scheme: dark)') : null;
  let longPressTimer = 0;
  let longPressed = false;

  const storedTheme = function () {
    try {
      const theme = localStorage.getItem(storageKey);
      return theme === 'light' || theme === 'dark' ? theme : '';
    } catch (_) {
      return '';
    }
  };
  const systemTheme = function () {
    return media && media.matches ? 'dark' : 'light';
  };
  const apply = function () {
    const theme = storedTheme();
    if (theme) {
      document.documentElement.dataset.theme = theme;
      button.dataset.themeState = theme;
      button.setAttribute('aria-label', 'Switch to ' + (theme === 'dark' ? 'light' : 'dark') + ' theme');
    } else {
      delete document.documentElement.dataset.theme;
      button.dataset.themeState = 'auto';
      button.setAttribute('aria-label', 'Theme follows system preference');
    }
  };
  const setTheme = function (theme) {
    try {
      localStorage.setItem(storageKey, theme);
    } catch (_) {}
    apply();
    setSyncStatus('Switched to ' + theme + '. Long-press to reset to system preferences.', 'is-current');
  };
  const clearTheme = function () {
    try {
      localStorage.removeItem(storageKey);
    } catch (_) {}
    apply();
    setSyncStatus('Theme follows system', 'is-current');
  };

  button.addEventListener('click', function () {
    if (longPressed) {
      longPressed = false;
      return;
    }
    const current = storedTheme() || systemTheme();
    setTheme(current === 'dark' ? 'light' : 'dark');
  });
  button.addEventListener('pointerdown', function () {
    longPressed = false;
    window.clearTimeout(longPressTimer);
    longPressTimer = window.setTimeout(function () {
      longPressed = true;
      clearTheme();
    }, 650);
  });
  for (const eventName of ['pointerup', 'pointercancel', 'pointerleave']) {
    button.addEventListener(eventName, function () {
      window.clearTimeout(longPressTimer);
    });
  }
  if (media) {
    media.addEventListener('change', apply);
  }
  apply();
}

function toggleUserMenu(button) {
  const menu = button.closest('.user-menu');
  const popover = menu ? menu.querySelector('[data-user-menu]') : null;
  if (!popover) return;
  const open = popover.hidden;
  closeUserMenus();
  popover.hidden = !open;
  button.setAttribute('aria-expanded', open ? 'true' : 'false');
}

function closeUserMenus() {
  for (const menu of document.querySelectorAll('.user-menu')) {
    const popover = menu.querySelector('[data-user-menu]');
    const button = menu.querySelector('[data-user-menu-toggle]');
    if (popover) popover.hidden = true;
    if (button) button.setAttribute('aria-expanded', 'false');
  }
}

function connectBgitEvents() {
  if (!window.EventSource) return;
  let source = null;
  let reconnecting = false;
  const connect = function () {
    source = new EventSource('/events');
    source.onopen = function () {
      if (reconnecting) {
        reconnecting = false;
        clearSyncStatus();
      }
    };
    source.addEventListener('git', function () {
      refreshRemoteState({refreshPullRequests: true});
    });
    source.addEventListener('whoami', function (event) {
      try {
        setWhoamiState(JSON.parse(event.data || 'null'));
      } catch (_) {}
    });
    source.addEventListener('assets', function () {
      setSyncStatus('Web assets changed. Reloading…', 'is-stale');
      window.location.reload();
    });
    source.onerror = function () {
      reconnecting = true;
      setSyncStatus('Lost connection to bgit@' + window.location.host + '... reconnecting.', 'is-error');
    };
  };
  connect();
  window.addEventListener('beforeunload', function () {
    if (source) source.close();
  });
}

async function refreshWhoamiState() {
  try {
    setWhoamiState(await fetchJSON('/api/me?refresh=1'));
  } catch (_) {}
}

async function refreshSettingsSections() {
  const root = document.querySelector('[data-settings-sections]');
  if (!root) return;
  const view = root.getAttribute('data-settings-view') || 'settings';
  root.classList.add('is-refreshing');
  try {
    const data = await fetchJSON('/api/settings-fragment?refresh=1&view=' + encodeURIComponent(view));
    if (data && typeof data.html === 'string') {
      root.innerHTML = data.html;
      applyCapabilityUI();
    }
  } catch (err) {
    root.innerHTML = '<section class="settings-section"><h2>Broker data unavailable</h2><div class="settings-error">' + escapeHTML(compactError(err)) + '</div></section>';
  } finally {
    root.classList.remove('is-refreshing');
  }
}

function setWhoamiState(value) {
  currentWhoami = value || null;
  document.documentElement.dataset.bgitRole = currentWhoami && currentWhoami.role ? currentWhoami.role : '';
  applyCapabilityUI();
  applyBoardOnlyMeFilter();
}

function hasCapability(name) {
  if (!name) return true;
  if (!currentWhoami || !currentWhoami.capabilities) return false;
  return currentWhoami.capabilities[name] === true;
}

function hasAnyCapability(names) {
  const parts = String(names || '').split(/\s+/).filter(Boolean);
  if (parts.length === 0) return true;
  return parts.some(hasCapability);
}

function applyCapabilityUI() {
  for (const el of document.querySelectorAll('[data-capability]')) {
    const allowed = hasCapability(el.getAttribute('data-capability') || '');
    const disabledMessage = 'Your current broker role does not allow this action.';
    if (el.matches('button, input, select, textarea')) {
      el.disabled = !allowed;
      el.title = allowed ? '' : disabledMessage;
    } else {
      el.classList.toggle('is-capability-disabled', !allowed);
      el.title = allowed ? '' : disabledMessage;
      for (const control of el.querySelectorAll('button, input, select, textarea')) control.disabled = !allowed;
    }
  }
  for (const el of document.querySelectorAll('[data-capability-any]')) {
    const allowed = hasAnyCapability(el.getAttribute('data-capability-any') || '');
    const disabledMessage = 'Your current broker role does not allow this action.';
    if (el.matches('button, input, select, textarea')) {
      el.disabled = !allowed;
      el.title = allowed ? '' : disabledMessage;
    } else {
      el.classList.toggle('is-capability-disabled', !allowed);
      el.title = allowed ? '' : disabledMessage;
      for (const control of el.querySelectorAll('button, input, select, textarea')) control.disabled = !allowed;
    }
  }
}

function currentBoardUsers() {
  if (!currentWhoami) return [];
  const users = [
    currentWhoami.user,
    currentWhoami.identity && currentWhoami.identity.user,
  ].map(function (value) {
    return String(value || '').trim().toLowerCase();
  }).filter(Boolean);
  return Array.from(new Set(users));
}

function toggleBoardOnlyMe(button) {
  if (button.disabled) return;
  const panel = button.closest('[data-board-panel]');
  if (!panel) return;
  panel.classList.toggle('is-only-me');
  applyBoardOnlyMeFilter();
}

function applyBoardOnlyMeFilter() {
  const panel = document.querySelector('[data-board-panel]');
  if (!panel) return;
  const button = panel.querySelector('[data-board-only-me]');
  const users = currentBoardUsers();
  const active = panel.classList.contains('is-only-me');
  if (button) {
    button.disabled = users.length === 0;
    button.classList.toggle('active', active);
    button.setAttribute('aria-pressed', active ? 'true' : 'false');
    button.title = users.length ? 'Show only stories assigned to you.' : 'Current broker user is not known yet.';
  }
  for (const card of panel.querySelectorAll('[data-story-id]')) {
    const assignee = String(card.getAttribute('data-story-assignee') || '').trim().toLowerCase();
    card.classList.toggle('is-filtered-out', active && (users.length === 0 || !users.includes(assignee)));
  }
  for (const addCard of panel.querySelectorAll('.story-card-add')) {
    addCard.classList.toggle('is-filtered-out', active);
  }
}

let pendingAvatarData = '';
let avatarCropState = null;

function setupUserSettingsPage() {
  const root = document.querySelector('[data-user-settings]');
  if (!root) return;
  loadUserProfile();
  const form = root.querySelector('[data-user-profile-form]');
  if (form) {
    form.addEventListener('submit', async function (event) {
      event.preventDefault();
      await saveUserProfile(form);
    });
  }
  root.querySelector('[data-user-avatar-file]')?.addEventListener('change', handleAvatarFile);
  root.querySelector('[data-avatar-crop-frame]')?.addEventListener('pointerdown', startAvatarPan);
  root.querySelector('[data-avatar-zoom-in]')?.addEventListener('click', function () { changeAvatarZoom(0.1); });
  root.querySelector('[data-avatar-zoom-out]')?.addEventListener('click', function () { changeAvatarZoom(-0.1); });
  root.querySelector('[data-avatar-apply]')?.addEventListener('click', applyAvatarCrop);
  root.querySelector('[data-avatar-cancel]')?.addEventListener('click', closeAvatarCropper);
}

async function loadUserProfile() {
  try {
    renderUserProfile(await fetchJSON('/api/user/profile'));
  } catch (err) {
    setSyncStatus(compactError(err), 'is-stale');
  }
}

function renderUserProfile(data) {
  const root = document.querySelector('[data-user-settings]');
  if (!root) return;
  const form = root.querySelector('[data-user-profile-form]');
  if (form && form.elements.bio) form.elements.bio.value = data?.profile?.bio || '';
  pendingAvatarData = data?.profile?.avatar || '';
  renderAvatarPreview(pendingAvatarData);
  renderUserKeys(Array.isArray(data?.keys) ? data.keys : []);
}

function renderAvatarPreview(value) {
  const preview = document.querySelector('[data-user-avatar-preview]');
  if (!preview) return;
  if (value) {
    preview.innerHTML = '<img alt="Profile image" src="' + escapeHTML(value) + '">';
  }
}

function renderUserKeys(keys) {
  const root = document.querySelector('[data-user-keys]');
  if (!root) return;
  if (!keys.length) {
    root.innerHTML = '<div class="empty">No SSH keys found for this user.</div>';
    return;
  }
  root.innerHTML = '<div class="user-key-list">' + keys.map(function (key) {
    const publicKey = String(key.public_key || '');
    const label = String(key.fingerprint || publicKey.slice(0, 48));
    const source = key.source ? '<span>' + escapeHTML(key.source) + '</span>' : '';
    return '<div class="user-key-row"><strong>' + escapeHTML(label) + '</strong>' + source + '<code>' + escapeHTML(publicKey) + '</code></div>';
  }).join('') + '</div>';
}

async function saveUserProfile(form) {
  try {
    const data = await postJSON('/api/user/profile', {bio: formValue(form, 'bio'), avatar: pendingAvatarData});
    renderUserProfile(data);
    setSyncStatus('Profile saved.', 'is-current');
  } catch (err) {
    setSyncStatus(compactError(err), 'is-stale');
  }
}

function handleAvatarFile(event) {
  const file = event.target.files && event.target.files[0];
  if (!file) return;
  const reader = new FileReader();
  reader.onload = function () {
    openAvatarCropper(String(reader.result || ''));
    event.target.value = '';
  };
  reader.readAsDataURL(file);
}

function openAvatarCropper(src) {
  const cropper = document.querySelector('[data-avatar-cropper]');
  const image = cropper ? cropper.querySelector('[data-avatar-crop-image]') : null;
  if (!cropper || !image) return;
  avatarCropState = {src, zoom: 1, x: 0, y: 0, dragging: false};
  image.onload = updateAvatarCropPreview;
  image.src = src;
  cropper.hidden = false;
  updateAvatarCropPreview();
}

function closeAvatarCropper() {
  const cropper = document.querySelector('[data-avatar-cropper]');
  if (cropper) cropper.hidden = true;
  avatarCropState = null;
}

function updateAvatarCropPreview() {
  const cropper = document.querySelector('[data-avatar-cropper]');
  const image = cropper ? cropper.querySelector('[data-avatar-crop-image]') : null;
  if (!cropper || !image || cropper.hidden || !avatarCropState || !image.complete) return;
  image.style.transform = 'translate(calc(-50% + ' + avatarCropState.x + 'px), calc(-50% + ' + avatarCropState.y + 'px)) scale(' + avatarCropState.zoom + ')';
}

function startAvatarPan(event) {
  if (!avatarCropState) return;
  event.preventDefault();
  const frame = event.currentTarget;
  avatarCropState.dragging = true;
  avatarCropState.dragStartX = event.clientX;
  avatarCropState.dragStartY = event.clientY;
  avatarCropState.startX = avatarCropState.x;
  avatarCropState.startY = avatarCropState.y;
  frame.setPointerCapture(event.pointerId);
  frame.addEventListener('pointermove', moveAvatarPan);
  frame.addEventListener('pointerup', endAvatarPan, {once: true});
  frame.addEventListener('pointercancel', endAvatarPan, {once: true});
}

function moveAvatarPan(event) {
  if (!avatarCropState || !avatarCropState.dragging) return;
  avatarCropState.x = avatarCropState.startX + event.clientX - avatarCropState.dragStartX;
  avatarCropState.y = avatarCropState.startY + event.clientY - avatarCropState.dragStartY;
  updateAvatarCropPreview();
}

function endAvatarPan(event) {
  const frame = event.currentTarget;
  if (avatarCropState) avatarCropState.dragging = false;
  frame.releasePointerCapture?.(event.pointerId);
  frame.removeEventListener('pointermove', moveAvatarPan);
}

function changeAvatarZoom(delta) {
  if (!avatarCropState) return;
  avatarCropState.zoom = Math.max(1, Math.min(3, Math.round((avatarCropState.zoom + delta) * 100) / 100));
  updateAvatarCropPreview();
}

function applyAvatarCrop() {
  const cropper = document.querySelector('[data-avatar-cropper]');
  const image = cropper ? cropper.querySelector('[data-avatar-crop-image]') : null;
  if (!cropper || !image || !image.complete) return;
  const canvas = document.createElement('canvas');
  const size = 320;
  canvas.width = size;
  canvas.height = size;
  const ctx = canvas.getContext('2d');
  const zoom = avatarCropState?.zoom || 1;
  const x = avatarCropState?.x || 0;
  const y = avatarCropState?.y || 0;
  const scale = Math.max(size / image.naturalWidth, size / image.naturalHeight) * zoom;
  const width = image.naturalWidth * scale;
  const height = image.naturalHeight * scale;
  ctx.drawImage(image, (size - width) / 2 + x, (size - height) / 2 + y, width, height);
  pendingAvatarData = canvas.toDataURL('image/png');
  renderAvatarPreview(pendingAvatarData);
  closeAvatarCropper();
}

async function fetchJSON(path) {
  const response = await fetch(path, {headers: {'accept': 'application/json'}});
  if (!response.ok) throw new Error(await response.text());
  return response.json();
}

async function postJSON(path, body) {
  const csrf = document.querySelector('meta[name="bgit-csrf-token"]')?.getAttribute('content') || '';
  const response = await fetch(path, {
    method: 'POST',
    headers: {'accept': 'application/json', 'content-type': 'application/json', 'x-bgit-csrf': csrf},
    body: JSON.stringify(body || {})
  });
  if (!response.ok) throw new Error(await response.text());
  return response.json();
}

let webActionInFlight = false;

function formValue(form, name) {
  const field = form.elements[name];
  return field ? String(field.value || '').trim() : '';
}

function formChecked(form, name) {
  const field = form.elements[name];
  return !!(field && field.checked);
}

async function handleSettingsForm(form) {
  const action = form.getAttribute('data-settings-form') || '';
  if (!hasCapability(form.getAttribute('data-capability') || '')) {
    setSyncStatus('Your current broker role does not allow this action.', 'is-stale');
    return;
  }
  const payload = {action};
  if (action === 'update-repo') {
    payload.description = formValue(form, 'description');
    payload.default_branch = formValue(form, 'default_branch');
    payload.visibility = formValue(form, 'visibility') || 'private';
    payload.read_only = formChecked(form, 'read_only');
    payload.issues_enabled = formChecked(form, 'issues_enabled');
  } else if (action === 'add-member') {
    payload.user = formValue(form, 'user');
    payload.role = formValue(form, 'role');
    if (!payload.user) {
      setSyncStatus('Username is required.', 'is-stale');
      return;
    }
  } else if (action === 'transfer-owner') {
    const ok = await confirmModal({title: 'Transfer ownership?', body: 'This creates a one-time command for the new owner to accept with their own SSH key.', confirm: 'Create command'});
    if (!ok) return;
  } else if (action === 'repo-rename') {
    payload.logical = formValue(form, 'logical');
    if (!payload.logical) {
      setSyncStatus('New repository name is required.', 'is-stale');
      return;
    }
    const ok = await confirmModal({title: 'Rename repository?', body: 'Rename this repository to ' + payload.logical + '.', confirm: 'OK'});
    if (!ok) return;
  } else if (action === 'repo-delete') {
    const expected = form.querySelector('[data-confirm-repo]')?.getAttribute('data-confirm-repo') || '';
    const actual = formValue(form, 'confirm');
    if (!expected || actual !== expected) {
      setSyncStatus('Type the repository name exactly to delete it.', 'is-stale');
      return;
    }
    const ok = await confirmModal({title: 'Delete repository?', body: 'This permanently deletes broker metadata, bucket contents, and the bucket for ' + expected + '.', confirm: 'Delete'});
    if (!ok) return;
  } else if (action === 'protect-upsert') {
    payload.ref = formValue(form, 'ref');
    payload.require_pr = formChecked(form, 'require_pr');
    payload.allow_overrides = formChecked(form, 'allow_overrides');
    if (!payload.ref) {
      setSyncStatus('Branch or ref is required.', 'is-stale');
      return;
    }
  }
  try {
    setSettingsBusy(true);
    const data = await postJSON('/api/actions/settings', payload);
    const command = data && data.broker && (data.broker.accept_command || data.broker.cancel_command);
    if (command) {
      await confirmModal({title: action === 'add-member' ? 'Invite command' : 'Ownership transfer command', body: command, confirm: 'OK'});
    }
    rememberSettingsStatus(settingsSuccessMessage(action, payload, form));
    window.location.reload();
  } catch (err) {
    setSyncStatus(compactError(err), 'is-stale');
  } finally {
    setSettingsBusy(false);
  }
}

async function handleSettingsAction(button) {
  const action = button.getAttribute('data-settings-action') || '';
  if (!hasCapability(button.closest('[data-capability]')?.getAttribute('data-capability') || button.getAttribute('data-capability') || '')) {
    setSyncStatus('Your current broker role does not allow this action.', 'is-stale');
    return;
  }
  const member = button.closest('[data-member-key]');
  const repoUser = button.closest('[data-repo-user]');
  const protection = button.closest('[data-protection-ref]');
  const payload = {action};
  let subject = '';
  let title = 'Apply settings change?';
  let body = 'This updates broker-managed repository settings.';
  if (member) {
    payload.key = member.getAttribute('data-member-key') || '';
    subject = member.querySelector('strong')?.textContent || 'member';
    if (!payload.key) {
      setSyncStatus('Member key is missing from the selected row.', 'is-stale');
      return;
    }
    if (action === 'remove-member') {
      title = 'Remove member?';
      body = 'Remove ' + subject + ' from this repository.';
    } else if (action === 'suspend-member') {
      title = 'Suspend member?';
      body = 'Suspend ' + subject + ' for this repository without removing the key.';
    } else if (action === 'unsuspend-member') {
      title = 'Unsuspend member?';
      body = 'Restore access for ' + subject + ' on this repository.';
    }
  }
  if (repoUser) {
    payload.user = repoUser.getAttribute('data-repo-user') || '';
    payload.user_id = repoUser.getAttribute('data-repo-user-id') || '';
    subject = repoUser.querySelector('strong')?.textContent || payload.user || 'user';
    if (!payload.user && !payload.user_id) {
      setSyncStatus('Repository user is missing from the selected row.', 'is-stale');
      return;
    }
    if (action === 'remove-repo-user') {
      title = 'Remove repository user?';
      body = 'Remove ' + subject + ' from this repository.';
    }
  }
  if (protection) {
    payload.ref = protection.getAttribute('data-protection-ref') || '';
    subject = payload.ref;
    if (!payload.ref) {
      setSyncStatus('Branch protection ref is missing from the selected row.', 'is-stale');
      return;
    }
    title = 'Remove branch protection?';
    body = 'Remove branch protection for ' + payload.ref + '.';
  }
  const ok = await confirmModal({title, body, confirm: 'OK'});
  if (!ok) return;
  try {
    setSettingsBusy(true);
    await postJSON('/api/actions/settings', payload);
    rememberSettingsStatus(settingsSuccessMessage(action, Object.assign({}, payload, {subject})));
    window.location.reload();
  } catch (err) {
    setSyncStatus(compactError(err), 'is-stale');
  } finally {
    setSettingsBusy(false);
  }
}

async function handleIssueForm(form) {
  const action = form.getAttribute('data-issue-form') || '';
  const panel = form.closest('[data-issue-id]');
  const payload = {action};
  if (panel) payload.id = Number(panel.getAttribute('data-issue-id') || 0);
  if (action === 'create') {
    payload.title = formValue(form, 'title');
    payload.body = formValue(form, 'body');
    if (!payload.title) {
      setSyncStatus('Issue title is required.', 'is-stale');
      return;
    }
  } else if (action === 'comment') {
    payload.comment = formValue(form, 'comment');
    if (!payload.id || !payload.comment) {
      setSyncStatus('Issue comment is required.', 'is-stale');
      return;
    }
  }
  try {
    await postJSON('/api/actions/issues', payload);
    window.location.reload();
  } catch (err) {
    setSyncStatus(compactError(err), 'is-stale');
  }
}

async function handleIssueAction(button) {
  const panel = button.closest('[data-issue-id]');
  const id = Number(panel?.getAttribute('data-issue-id') || 0);
  const action = button.getAttribute('data-issue-action') || '';
  if (!id || !action) return;
  try {
    await postJSON('/api/actions/issues', {action, id});
    window.location.reload();
  } catch (err) {
    setSyncStatus(compactError(err), 'is-stale');
  }
}

async function handleBoardForm(form) {
  const action = form.getAttribute('data-board-form') || '';
  const card = form.closest('[data-story-id]');
  const payload = {action};
  if (card) payload.id = Number(card.getAttribute('data-story-id') || 0);
  if (action === 'comment') {
    payload.comment = formValue(form, 'comment');
    if (!payload.id || !payload.comment) {
      setSyncStatus('Story comment is required.', 'is-stale');
      return;
    }
  } else if (action === 'edit') {
    payload.body = formValue(form, 'body');
    if (!payload.id || !payload.body) {
      setSyncStatus('Story is required.', 'is-stale');
      return;
    }
  } else {
    payload.body = formValue(form, 'body');
    payload.lane = 'backlog';
    if (!payload.body) {
      setSyncStatus('Story is required.', 'is-stale');
      return;
    }
  }
  try {
    await postJSON('/api/actions/board', payload);
    window.location.reload();
  } catch (err) {
    setSyncStatus(compactError(err), 'is-stale');
  }
}

async function handleCIForm(form) {
  if (!hasCapability(form.getAttribute('data-capability') || '')) {
    setSyncStatus('Your current broker role does not allow this action.', 'is-stale');
    return;
  }
  const payload = formDataObject(form);
  payload.action = 'run';
  const submit = form.querySelector('button[type="submit"]');
  if (submit) submit.disabled = true;
  try {
    await postJSON('/api/actions/ci', payload);
    window.location.reload();
  } catch (err) {
    setSyncStatus(err.message || String(err), 'is-stale');
  } finally {
    if (submit) submit.disabled = false;
  }
}

function ciStatusTerminal(status) {
  return ['passed', 'failed', 'cancelled', 'timed_out'].includes(String(status || '').toLowerCase());
}

function ciRunRow(run) {
  const status = run.status || 'queued';
  const ref = String(run.ref || '').replace(/^refs\/heads\//, '');
  const commit = String(run.commit || '').slice(0, 12);
  const provider = String(run.provider || 'ci').toUpperCase();
  const message = run.message ? '<small>' + escapeHTML(run.message) + '</small>' : '';
  const open = run.url ? '<a class="button-link" href="' + escapeHTML(run.url) + '" target="_blank" rel="noreferrer">Open</a>' : '';
  return '<div class="ci-run-row" data-ci-run-id="' + Number(run.id || 0) + '" data-ci-status="' + escapeHTML(status) + '"><div><strong>#' + Number(run.id || 0) + ' ' + escapeHTML(status) + '</strong><span>' + escapeHTML(provider + ' · ' + ref + ' · ' + commit + ' · ' + (run.config || '')) + '</span>' + message + '<pre class="ci-run-log" data-ci-log hidden></pre></div>' + open + '</div>';
}

async function refreshCIRuns() {
  const list = document.querySelector('[data-ci-runs]');
  if (!list) return;
  try {
    const data = await postJSON('/api/actions/ci', {action: 'list'});
    if (Array.isArray(data.runs)) {
      list.innerHTML = data.runs.map(ciRunRow).join('');
      streamVisibleCILogs();
      if (data.runs.some((run) => !ciStatusTerminal(run.status || 'queued'))) {
        window.setTimeout(refreshCIRuns, 3000);
      }
    }
  } catch (_) {}
}

async function streamVisibleCILogs() {
  for (const row of document.querySelectorAll('[data-ci-run-id]')) {
    const id = Number(row.getAttribute('data-ci-run-id') || 0);
    if (!id) continue;
    try {
      const data = await postJSON('/api/actions/ci', {action: 'logs', id});
      const log = row.querySelector('[data-ci-log]');
      if (log && data.logs) {
        log.hidden = false;
        log.textContent = data.logs;
        log.scrollTop = log.scrollHeight;
      }
      if (data.run && data.run.status) row.setAttribute('data-ci-status', data.run.status);
    } catch (_) {}
  }
}

async function handleBoardAction(button) {
  const action = button.getAttribute('data-board-action') || '';
  if (action === 'new') {
    await createBoardStory();
    return;
  }
  const card = button.closest('[data-story-id]');
  const id = Number(card?.getAttribute('data-story-id') || 0);
  if (!id || !action) return;
  if (action === 'edit') {
    await editBoardStory(card, id);
    return;
  }
  try {
    await postJSON('/api/actions/board', {action, id});
    window.location.reload();
  } catch (err) {
    setSyncStatus(compactError(err), 'is-stale');
  }
}

async function editBoardStory(card, id) {
  if (!hasCapability('push')) {
    setSyncStatus('Your current broker role does not allow this action.', 'is-stale');
    return;
  }
  const current = (card.querySelector('.story-body') || card.querySelector('p'))?.textContent || '';
  const body = await promptModal({
    cardClass: 'story-modal-card',
    confirm: 'Save story',
    inputLabel: 'Story',
    multiline: true,
    placeholder: 'As a contributor, I want to describe the work in one clear sentence, so that the team understands the value.',
    rows: 6,
    value: current,
  });
  if (body === false) return;
  const story = String(body || '').trim();
  if (!story) {
    setSyncStatus('Story is required.', 'is-stale');
    return;
  }
  try {
    await postJSON('/api/actions/board', {action: 'edit', id, body: story});
    window.location.reload();
  } catch (err) {
    setSyncStatus(compactError(err), 'is-stale');
  }
}

async function createBoardStory() {
  if (!hasCapability('push')) {
    setSyncStatus('Your current broker role does not allow this action.', 'is-stale');
    return;
  }
  const body = await promptModal({
    cardClass: 'story-modal-card',
    confirm: 'Add story',
    inputLabel: 'Story',
    multiline: true,
    placeholder: 'As a contributor, I want to describe the work in one clear sentence, so that the team understands the value.',
    rows: 6,
  });
  if (body === false) return;
  const story = String(body || '').trim();
  if (!story) {
    setSyncStatus('Story is required.', 'is-stale');
    return;
  }
  try {
    await postJSON('/api/actions/board', {action: 'create', body: story, lane: 'backlog'});
    window.location.reload();
  } catch (err) {
    setSyncStatus(compactError(err), 'is-stale');
  }
}

async function handleBoardLaneChange(select) {
  const card = select.closest('[data-story-id]');
  const id = Number(card?.getAttribute('data-story-id') || 0);
  const lane = select.value;
  if (card?.classList.contains('is-committing')) {
    hideBoardLaneSelect(select);
    return;
  }
  if (!id || !lane) return;
  const moved = await moveBoardStory(id, lane, null);
  if (moved) hideBoardLaneSelect(select);
}

async function moveBoardStory(id, lane, afterID) {
  const card = findBoardStoryCard(id);
  const targetLane = findBoardLane(lane);
  if (card && targetLane) {
    return await moveBoardStoryOptimistically(card, targetLane, lane, afterID);
  }
  try {
    const payload = {action: 'move', id, lane};
    if (Number.isInteger(afterID)) payload.after_id = afterID;
    await postJSON('/api/actions/board', payload);
    window.location.reload();
    return true;
  } catch (err) {
    setSyncStatus(compactError(err), 'is-stale');
    return false;
  }
}

async function moveBoardStoryOptimistically(card, targetLane, lane, afterID) {
  const id = Number(card.getAttribute('data-story-id') || 0);
  const fromLane = card.getAttribute('data-story-lane') || '';
  if (!id || !lane || fromLane === lane || card.classList.contains('is-committing')) return true;
  const originalParent = card.parentElement;
  const originalNextSibling = card.nextElementSibling;
  const laneSelect = card.querySelector('[data-board-lane]');

  insertBoardCardAfter(targetLane, card, afterID);
  card.setAttribute('data-story-lane', lane);
  if (laneSelect) laneSelect.value = lane;
  setBoardStoryCommitting(card, true);
  try {
    const payload = {action: 'move', id, lane};
    if (Number.isInteger(afterID)) payload.after_id = afterID;
    await postJSON('/api/actions/board', payload);
    setBoardStoryCommitting(card, false);
    return true;
  } catch (err) {
    if (originalParent) {
      originalParent.insertBefore(card, originalNextSibling && originalNextSibling.parentElement === originalParent ? originalNextSibling : null);
    }
    card.setAttribute('data-story-lane', fromLane);
    if (laneSelect) laneSelect.value = fromLane;
    setBoardStoryCommitting(card, false);
    setSyncStatus(compactError(err), 'is-stale');
    return false;
  }
}

async function reorderBoardStory(id, lane, afterID) {
  const card = findBoardStoryCard(id);
  const targetLane = findBoardLane(lane);
  if (!card || !targetLane || card.classList.contains('is-committing')) return false;
  const originalParent = card.parentElement;
  const originalNextSibling = card.nextElementSibling;
  insertBoardCardAfter(targetLane, card, afterID);
  setBoardStoryCommitting(card, true);
  try {
    await postJSON('/api/actions/board', {action: 'reorder', id, lane, after_id: afterID || 0});
    setBoardStoryCommitting(card, false);
    return true;
  } catch (err) {
    if (originalParent) {
      originalParent.insertBefore(card, originalNextSibling && originalNextSibling.parentElement === originalParent ? originalNextSibling : null);
    }
    setBoardStoryCommitting(card, false);
    setSyncStatus(compactError(err), 'is-stale');
    return false;
  }
}

function insertBoardCardAfter(lane, card, afterID) {
  if (!Number.isInteger(afterID)) {
    lane.appendChild(card);
    return;
  }
  if (!afterID) {
    const first = firstBoardStoryCard(lane, card);
    lane.insertBefore(card, first);
    return;
  }
  const after = Array.from(lane.querySelectorAll('[data-story-id]')).find((candidate) =>
    Number(candidate.getAttribute('data-story-id') || 0) === Number(afterID || 0)
  );
  lane.insertBefore(card, after ? after.nextElementSibling : null);
}

function firstBoardStoryCard(lane, except) {
  for (const candidate of lane.querySelectorAll('[data-story-id]')) {
    if (candidate !== except) return candidate;
  }
  return null;
}

function boardDropAfterID(lane, clientY, draggedID) {
  let afterID = 0;
  for (const card of lane.querySelectorAll('[data-story-id]')) {
    const id = Number(card.getAttribute('data-story-id') || 0);
    if (!id || id === Number(draggedID || 0)) continue;
    const rect = card.getBoundingClientRect();
    if (clientY < rect.top + rect.height / 2) return afterID;
    afterID = id;
  }
  return afterID;
}

function hideBoardLaneSelect(select) {
  const actions = select.closest('.story-actions, .story-detail-actions');
  select.hidden = true;
  actions?.classList.remove('is-moving');
  actions?.closest('[data-story-id]')?.classList.remove('is-menu-open');
}

function cancelBoardMove(select) {
  const card = select.closest('[data-story-id]');
  const currentLane = card?.getAttribute('data-story-lane') || '';
  if (currentLane) select.value = currentLane;
  hideBoardLaneSelect(select);
}

function setBoardStoryCommitting(card, committing) {
  card.classList.toggle('is-committing', !!committing);
  card.setAttribute('aria-busy', committing ? 'true' : 'false');
  let spinner = card.querySelector('[data-story-commit-spinner]');
  if (committing && !spinner) {
    spinner = document.createElement('span');
    spinner.className = 'story-commit-spinner';
    spinner.setAttribute('data-story-commit-spinner', '');
    spinner.setAttribute('aria-label', 'Committing');
    const title = card.querySelector('.story-card-title');
    title?.insertBefore(spinner, title.querySelector('.story-actions'));
  } else if (!committing && spinner) {
    spinner.remove();
  }
  for (const control of card.querySelectorAll('button, select, textarea, input')) {
    control.disabled = !!committing;
  }
  if (!committing) applyCapabilityUI();
}

function findBoardStoryCard(id) {
  for (const card of document.querySelectorAll('[data-story-id]')) {
    if (Number(card.getAttribute('data-story-id') || 0) === Number(id || 0)) return card;
  }
  return null;
}

function findBoardLane(lane) {
  for (const el of document.querySelectorAll('[data-board-drop-lane]')) {
    if ((el.getAttribute('data-board-drop-lane') || '') === lane) return el;
  }
  return null;
}

function clearBoardDropTargets() {
  for (const lane of document.querySelectorAll('[data-board-drop-lane].is-drop-target')) {
    lane.classList.remove('is-drop-target');
  }
}

async function handleBoardReassignChange(select) {
  const card = select.closest('[data-story-id]');
  const id = Number(card?.getAttribute('data-story-id') || 0);
  if (!id) return;
  const value = select.value;
  const assignee = value === '__unassigned__' ? '' : value;
  try {
    await postJSON('/api/actions/board', {action: 'assign', id, assignee});
    window.location.reload();
  } catch (err) {
    setSyncStatus(compactError(err), 'is-stale');
  }
}

function cancelBoardReassign(select) {
  const actions = select.closest('.story-actions, .story-detail-actions');
  const toggle = actions ? actions.querySelector('[data-board-reassign-toggle]') : null;
  select.hidden = true;
  actions?.classList.remove('is-reassigning');
  actions?.closest('[data-story-id]')?.classList.remove('is-menu-open');
  if (toggle) toggle.focus();
}

function setSettingsBusy(busy) {
  for (const el of document.querySelectorAll('[data-settings-root] button, [data-settings-root] input, [data-settings-root] textarea, [data-settings-root] select')) {
    el.disabled = !!busy;
  }
  if (!busy) applyCapabilityUI();
}

function settingsSuccessMessage(action, payload, form) {
  const subject = payload.subject || payload.user || payload.ref || form?.querySelector('input[name="user"]')?.value || '';
  if (action === 'update-repo') return 'Repository settings saved.';
  if (action === 'add-member') return 'Created invite for ' + subject + '.';
  if (action === 'transfer-owner') return 'Ownership transfer is pending.';
  if (action === 'repo-rename') return 'Repository renamed.';
  if (action === 'repo-delete') return 'Repository deleted.';
  if (action === 'suspend-member') return 'Suspended ' + subject + '.';
  if (action === 'unsuspend-member') return 'Unsuspended ' + subject + '.';
  if (action === 'remove-member') return 'Removed ' + subject + '.';
  if (action === 'remove-repo-user') return 'Removed ' + subject + '.';
  if (action === 'protect-upsert') return 'Protected ' + subject + '.';
  if (action === 'protect-remove') return 'Removed protection for ' + subject + '.';
  return 'Settings updated.';
}

function rememberSettingsStatus(message) {
  try {
    window.sessionStorage.setItem('bgit.settingsStatus', message);
  } catch (_) {}
}

function restoreSettingsStatus() {
  let message = '';
  try {
    message = window.sessionStorage.getItem('bgit.settingsStatus') || '';
    window.sessionStorage.removeItem('bgit.settingsStatus');
  } catch (_) {}
  if (message) setSyncStatus(message, 'is-current');
}

async function handleWebAction(action, trigger) {
  if (webActionInFlight) return;
  try {
    if (action === 'stage') {
      const path = trigger ? trigger.getAttribute('data-path') : '';
      if (!path) return;
      setWebActionsBusy(true, 'STAGING');
      setRemoteSyncStatus('syncing', 'Synchronising');
      const data = await postJSON('/api/actions/stage', {path});
      currentWebState = data.state || null;
      applyRepositoryState(currentWebState);
      reconcileRemoteState(currentWebState);
      setSyncStatus('Staged ' + path + '.', 'is-current');
      return;
    }
    if (action === 'unstage') {
      const path = trigger ? trigger.getAttribute('data-path') : '';
      if (!path) return;
      setWebActionsBusy(true, 'UNSTAGING');
      const data = await postJSON('/api/actions/unstage', {path});
      currentWebState = data.state || null;
      applyRepositoryState(currentWebState);
      reconcileRemoteState(currentWebState);
      setSyncStatus('Unstaged ' + path + '.', 'is-current');
      return;
    }
    if (action === 'discard') {
      const path = trigger ? trigger.getAttribute('data-path') : '';
      if (!path) return;
      const ok = await confirmModal({
        title: 'Checkout file?',
        body: 'Discard local changes for ' + path + ' and restore it from the remote branch when available.',
        confirm: 'OK'
      });
      if (!ok) return;
      setWebActionsBusy(true, 'CHECKING OUT');
      const data = await postJSON('/api/actions/discard', {path});
      currentWebState = data.state || null;
      applyRepositoryState(currentWebState);
      reconcileRemoteState(currentWebState);
      setSyncStatus('Checked out ' + path + '.', 'is-current');
      return;
    }
    if (action === 'commit') {
      const stagedFiles = currentWebState && Array.isArray(currentWebState.staged_files) ? currentWebState.staged_files : [];
      const message = await promptModal({
        title: 'Commit staged changes',
        body: 'Commit the staged changes on the current branch.',
        files: stagedFiles,
        inputLabel: 'Commit message',
        confirm: 'Commit',
        required: true
      });
      if (!message) return;
      setWebActionsBusy(true, 'COMMITTING');
      setRemoteSyncStatus('syncing', 'Synchronising');
      const data = await postJSON('/api/actions/commit', {message});
      currentWebState = data.state || null;
      setSyncStatus('Committed local changes.', 'is-current');
      reloadLocalView();
      return;
    }
    if (action === 'push') {
      setWebActionsBusy(true, 'PUSHING');
      setRemoteSyncStatus('syncing', 'Synchronising');
      const data = await postJSON('/api/actions/push', {});
      currentWebState = data.state || null;
      applyRepositoryState(currentWebState);
      if (currentWebState && Number(currentWebState.ahead || 0) > 0) {
        throw new Error('Push did not complete; local branch is still ahead of remote.');
      }
      reconcileRemoteState(currentWebState);
      setSyncStatus('Push confirmed.', 'is-current');
      return;
    }
    if (action === 'uncommit') {
      const ok = await confirmModal({
        title: 'Uncommit local commits?',
        body: 'Move unpushed commits back into staged changes. Nothing will be changed on the remote.',
        confirm: 'Uncommit'
      });
      if (!ok) return;
      setWebActionsBusy(true, 'UNCOMMITTING');
      setRemoteSyncStatus('syncing', 'Synchronising');
      const data = await postJSON('/api/actions/uncommit', {});
      currentWebState = data.state || null;
      applyRepositoryState(currentWebState);
      reconcileRemoteState(currentWebState);
      setSyncStatus('Uncommitted local commits.', 'is-current');
      return;
    }
    if (action === 'pull') {
      const ok = await confirmModal({
        title: 'Pull remote changes?',
        body: 'Remote has commits that are not in your local branch. Pull them into this working tree?',
        confirm: 'Pull'
      });
      if (!ok) return;
      setWebActionsBusy(true, 'PULLING');
      setRemoteSyncStatus('syncing', 'Synchronising');
      const data = await postJSON('/api/actions/pull', {});
      currentWebState = data.state || null;
      setSyncStatus('Pulled remote changes.', 'is-current');
      reloadLocalView();
    }
  } catch (err) {
    setRemoteSyncStatus('error', compactError(err));
    setSyncStatus(compactError(err), 'is-stale');
  } finally {
    setWebActionsBusy(false);
  }
}

function confirmModal(options) {
  return modalDialog(options).then(function (value) { return value === true; });
}

function promptModal(options) {
  return modalDialog(Object.assign({}, options, {prompt: true}));
}

async function showInlineDiff(trigger) {
  const path = trigger.getAttribute('data-path') || '';
  const mode = trigger.getAttribute('data-mode') || 'worktree';
  const diffURL = trigger.getAttribute('data-diff-url') || (path ? '/api/diff?path=' + encodeURIComponent(path) + '&mode=' + encodeURIComponent(mode) : '');
  if (!diffURL) return;
  const anchor = trigger.closest('[data-file-row]') || trigger.closest('[data-commit-row]') || trigger.closest('.pr-detail-header');
  if (!anchor) return;
  const existing = anchor.nextElementSibling && anchor.nextElementSibling.matches('[data-inline-diff-row]') ? anchor.nextElementSibling : null;
  if (existing) {
    existing.remove();
    trigger.classList.remove('is-active');
    trigger.setAttribute('aria-expanded', 'false');
    return;
  }
  for (const open of document.querySelectorAll('[data-inline-diff-row]')) open.remove();
  for (const active of document.querySelectorAll('[data-web-diff].is-active')) {
    active.classList.remove('is-active');
    active.setAttribute('aria-expanded', 'false');
  }
  trigger.disabled = true;
  try {
    const data = await fetchJSON(diffURL);
    const title = trigger.getAttribute('data-diff-title') || path || 'Diff';
    const subtitle = trigger.getAttribute('data-diff-subtitle') || (mode === 'staged' ? 'Staged changes' : 'Unstaged changes');
    const diffRow = inlineDiffElement(anchor, title, subtitle, data.html || visualDiffHTML(data.diff || ''), !!data.html);
    anchor.insertAdjacentElement('afterend', diffRow);
    trigger.classList.add('is-active');
    trigger.setAttribute('aria-expanded', 'true');
  } catch (err) {
    setRemoteSyncStatus('error', compactError(err));
    setSyncStatus(compactError(err), 'is-stale');
  } finally {
    trigger.disabled = false;
  }
}

function inlineDiffElement(anchor, title, subtitle, content, isHTML) {
  let el;
  let inner;
  if (anchor.tagName === 'TR') {
    el = document.createElement('tr');
    inner = '<td colspan="3">' + inlineDiffShellHTML(title, subtitle, content, isHTML) + '</td>';
  } else if (anchor.tagName === 'LI') {
    el = document.createElement('li');
    inner = inlineDiffShellHTML(title, subtitle, content, isHTML);
  } else {
    el = document.createElement('section');
    inner = inlineDiffShellHTML(title, subtitle, content, isHTML);
  }
  el.className = 'inline-diff-row';
  el.setAttribute('data-inline-diff-row', '');
  el.innerHTML = inner;
  return el;
}

function inlineDiffShellHTML(title, subtitle, content, isHTML) {
  const body = isHTML ? String(content || '') : visualDiffHTML(content || '');
  return '<div class="inline-diff-shell"><div class="inline-diff-header"><strong>' + escapeHTML(title) + '</strong><span>' + escapeHTML(subtitle || '') + '</span></div>' + body + '</div>';
}

function visualDiffHTML(diff) {
  const files = parseUnifiedDiff(diff || '');
  if (!files.length) return '<div class="empty">No diff available.</div>';
  return '<div class="visual-diff">' + files.map(function (file) {
    return '<section class="visual-diff-file"><div class="visual-diff-title">' + escapeHTML(file.path || 'Changed file') + '</div><div class="visual-diff-grid"><div class="visual-diff-heading visual-diff-line-heading"></div><div class="visual-diff-heading">Before</div><div class="visual-diff-heading visual-diff-line-heading"></div><div class="visual-diff-heading">After</div>' + file.rows.map(diffRowHTML).join('') + '</div></section>';
  }).join('') + '</div>';
}

function parseUnifiedDiff(diff) {
  const files = [];
  let current = null;
  let pendingDeletes = [];
  let oldLine = 0;
  let newLine = 0;
  function flushDeletes() {
    if (!current || !pendingDeletes.length) return;
    for (const line of pendingDeletes) current.rows.push({kind: 'del', left: line.text, right: '', oldLine: line.oldLine, newLine: ''});
    pendingDeletes = [];
  }
  for (const raw of String(diff || '').split(/\r?\n/)) {
    if (raw.startsWith('diff --git ')) {
      flushDeletes();
      const match = raw.match(/^diff --git a\/(.+?) b\/(.+)$/);
      current = {path: match ? match[2] : 'Changed file', rows: []};
      files.push(current);
      continue;
    }
    if (!current && raw !== '') {
      current = {path: 'Changed file', rows: []};
      files.push(current);
    }
    if (!current) continue;
    if (raw.startsWith('+++ ') || raw.startsWith('--- ') || raw.startsWith('index ') || raw.startsWith('new file mode') || raw.startsWith('deleted file mode')) continue;
    if (raw.startsWith('@@')) {
      flushDeletes();
      const hunk = parseHunkStart(raw);
      oldLine = hunk.oldLine;
      newLine = hunk.newLine;
      current.rows.push({kind: 'hunk', left: raw, right: raw, oldLine: '', newLine: ''});
      continue;
    }
    if (raw.startsWith('-')) {
      pendingDeletes.push({text: raw.slice(1), oldLine: oldLine});
      oldLine += 1;
      continue;
    }
    if (raw.startsWith('+')) {
      const added = raw.slice(1);
      if (pendingDeletes.length) {
        const deleted = pendingDeletes.shift();
        current.rows.push({kind: 'change', left: deleted.text, right: added, oldLine: deleted.oldLine, newLine: newLine});
      } else {
        current.rows.push({kind: 'add', left: '', right: added, oldLine: '', newLine: newLine});
      }
      newLine += 1;
      continue;
    }
    flushDeletes();
    if (raw === '\\ No newline at end of file') {
      current.rows.push({kind: 'note', left: raw, right: raw, oldLine: '', newLine: ''});
    } else {
      const text = raw.startsWith(' ') ? raw.slice(1) : raw;
      current.rows.push({kind: 'same', left: text, right: text, oldLine: oldLine, newLine: newLine});
      oldLine += 1;
      newLine += 1;
    }
  }
  flushDeletes();
  for (const file of files) {
    if (!file.rows.length) file.rows.push({kind: 'note', left: 'No textual changes.', right: 'No textual changes.', oldLine: '', newLine: ''});
  }
  return files;
}

function parseHunkStart(line) {
  const match = String(line || '').match(/^@@ -(\d+)(?:,\d+)? \+(\d+)(?:,\d+)? @@/);
  return {
    oldLine: match ? Number(match[1]) : 0,
    newLine: match ? Number(match[2]) : 0
  };
}

function diffRowHTML(row) {
  if (row.kind === 'hunk' || row.kind === 'note') {
    return '<div class="visual-diff-divider visual-diff-' + row.kind + '"><span>' + escapeHTML(formatDiffDivider(row.left)) + '</span></div>';
  }
  if (row.kind === 'change') {
    const segments = inlineChangedSegments(row.left, row.right);
    return '<div class="visual-diff-row visual-diff-change"><div class="visual-diff-line-number">' + escapeHTML(row.oldLine || '') + '</div><pre>' + segments.left + '</pre><div class="visual-diff-line-number">' + escapeHTML(row.newLine || '') + '</div><pre>' + segments.right + '</pre></div>';
  }
  return '<div class="visual-diff-row visual-diff-' + row.kind + '"><div class="visual-diff-line-number">' + escapeHTML(row.oldLine || '') + '</div><pre>' + diffCellHTML(row.left, row.kind === 'del' ? 'deleted' : 'same') + '</pre><div class="visual-diff-line-number">' + escapeHTML(row.newLine || '') + '</div><pre>' + diffCellHTML(row.right, row.kind === 'add' ? 'added' : 'same') + '</pre></div>';
}

function revealDiffContext(button) {
  const divider = button.closest('.visual-diff-divider');
  if (!divider) return;
  const direction = button.getAttribute('data-diff-context');
  const hiddenRows = hiddenContextRowsForDivider(divider, direction);
  const rows = direction === 'up' ? hiddenRows.slice(-20) : hiddenRows.slice(0, 20);
  for (const row of rows) {
    row.hidden = false;
    row.removeAttribute('data-hidden-context');
  }
  if (rows.length > 0) {
    if (direction === 'up') {
      rows[0].before(divider);
    } else {
      rows[rows.length - 1].after(divider);
    }
  }
  if (hiddenContextRowsForDivider(divider, direction).length === 0) {
    button.disabled = true;
    button.setAttribute('aria-disabled', 'true');
  }
}

function hiddenContextRowsForDivider(divider, direction) {
  const rows = [];
  if (direction === 'up') {
    let node = divider.previousElementSibling;
    while (node && !node.classList.contains('visual-diff-divider')) {
      if (node.hasAttribute('data-hidden-context')) rows.push(node);
      node = node.previousElementSibling;
    }
    rows.reverse();
    return rows;
  }
  let node = divider.nextElementSibling;
  while (node && !node.classList.contains('visual-diff-divider')) {
    if (node.hasAttribute('data-hidden-context')) rows.push(node);
    node = node.nextElementSibling;
  }
  return rows;
}

function formatDiffDivider(line) {
  const match = String(line || '').match(/^@@ -(\d+)(?:,(\d+))? \+(\d+)(?:,(\d+))? @@/);
  if (!match) return line || '';
  const oldStart = Number(match[1]);
  const oldCount = Number(match[2] || 1);
  const newStart = Number(match[3]);
  const newCount = Number(match[4] || 1);
  return 'Lines ' + lineRangeLabel(oldStart, oldCount) + ' -> ' + lineRangeLabel(newStart, newCount);
}

function lineRangeLabel(start, count) {
  if (!count || count <= 1) return String(start);
  return String(start) + '-' + String(start + count - 1);
}

function diffCellHTML(text, kind) {
  if (!text) return '';
  const value = escapeHTML(text);
  if (kind === 'deleted' || kind === 'added') return '<span class="diff-change ' + kind + '">' + value + '</span>';
  return value;
}

function inlineChangedSegments(left, right) {
  left = String(left || '');
  right = String(right || '');
  let prefix = 0;
  while (prefix < left.length && prefix < right.length && left[prefix] === right[prefix]) prefix += 1;
  let suffix = 0;
  while (
    suffix < left.length - prefix &&
    suffix < right.length - prefix &&
    left[left.length - 1 - suffix] === right[right.length - 1 - suffix]
  ) {
    suffix += 1;
  }
  const oldEnd = left.length - suffix;
  const newEnd = right.length - suffix;
  return {
    left: escapeHTML(left.slice(0, prefix)) + '<span class="diff-change deleted">' + escapeHTML(left.slice(prefix, oldEnd) || ' ') + '</span>' + escapeHTML(left.slice(oldEnd)),
    right: escapeHTML(right.slice(0, prefix)) + '<span class="diff-change added">' + escapeHTML(right.slice(prefix, newEnd) || ' ') + '</span>' + escapeHTML(right.slice(newEnd))
  };
}

async function handlePullRequestAction(trigger) {
  const panel = trigger.closest('[data-pr-id]');
  if (!panel) return;
  const id = Number(panel.getAttribute('data-pr-id') || 0);
  const action = trigger.getAttribute('data-pr-action') || '';
  const textarea = panel.querySelector('[data-pr-comment]');
  const deleteBranch = panel.querySelector('[data-pr-delete-branch]');
  const comment = textarea ? textarea.value.trim() : '';
  try {
    let confirmed = true;
    if (action === 'merge') {
      confirmed = await confirmModal({
        title: 'Merge pull request?',
        body: deleteBranch && deleteBranch.checked ? 'Merge this pull request and delete the source branch afterwards.' : 'Merge this pull request into the target branch.',
        confirm: 'Merge'
      });
    } else if (action === 'reject') {
      confirmed = await confirmModal({
        title: 'Request changes?',
        body: comment ? 'Submit this review as changes requested.' : 'Submit a changes requested review without a note.',
        confirm: 'Request changes'
      });
    } else if (action === 'close') {
      confirmed = await confirmModal({
        title: 'Close pull request?',
        body: 'Close this pull request without merging it.',
        confirm: 'Close PR'
      });
    } else if (action === 'reopen') {
      confirmed = await confirmModal({
        title: 'Reopen pull request?',
        body: 'Reopen this pull request so it can be reviewed and merged again.',
        confirm: 'Reopen PR'
      });
    }
    if (!confirmed) return;
    setPullRequestActionsBusy(panel, true, action);
    const data = await postJSON('/api/actions/pr', {
      id,
      action,
      comment,
      delete_branch: !!(deleteBranch && deleteBranch.checked)
    });
    if (Array.isArray(data.prs)) updatePullRequestUI(data.prs);
    setSyncStatus('Pull request updated.', 'is-current');
    window.location.reload();
  } catch (err) {
    setSyncStatus(compactError(err), 'is-stale');
  } finally {
    setPullRequestActionsBusy(panel, false);
  }
}

function setupPullRequestCreatePage() {
  const form = document.querySelector('[data-pr-create-form]');
  if (!form) return;
  form.querySelector('[data-pr-create-source]')?.addEventListener('change', updatePullRequestCreateSummary);
  form.querySelector('[data-pr-create-target]')?.addEventListener('change', updatePullRequestCreateSummary);
  updatePullRequestCreateSummary();
}

let prCreatePreviewTimer = 0;

function updatePullRequestCreateSummary() {
  const form = document.querySelector('[data-pr-create-form]');
  if (!form) return;
  const source = form.elements.source?.selectedOptions?.[0]?.textContent || shortRefName(form.elements.source?.value || '');
  const target = form.elements.target?.selectedOptions?.[0]?.textContent || shortRefName(form.elements.target?.value || '');
  const summary = form.querySelector('[data-pr-create-summary]');
  if (!summary) return;
  if (form.elements.source?.value === form.elements.target?.value) {
    summary.classList.add('is-error');
    summary.textContent = 'There is nothing to compare when base and compare are the same branch.';
    renderPullRequestCreateDiff('<div class="empty">Choose different branches to preview the diff.</div>');
    return;
  }
  summary.classList.remove('is-error');
  summary.textContent = 'Compare ' + source + ' into ' + target + '.';
  window.clearTimeout(prCreatePreviewTimer);
  prCreatePreviewTimer = window.setTimeout(loadPullRequestCreateDiff, 180);
}

async function loadPullRequestCreateDiff() {
  const form = document.querySelector('[data-pr-create-form]');
  if (!form) return;
  const source = form.elements.source?.value || '';
  const target = form.elements.target?.value || '';
  if (!source || !target || source === target) return;
  renderPullRequestCreateDiff('<div class="empty">Loading diff...</div>');
  try {
    const data = await fetchJSON('/api/diff?source=' + encodeURIComponent(source) + '&target=' + encodeURIComponent(target));
    const state = data.mergeable ? '<div class="pr-mergeability is-clean">Mergeable without conflicts</div>' : '<div class="pr-mergeability is-conflict">' + escapeHTML(data.conflict || 'Potential merge conflicts') + '</div>';
    renderPullRequestCreateDiff(state + (data.html || '<div class="empty">No diff available.</div>'));
  } catch (err) {
    renderPullRequestCreateDiff('<div class="settings-error">' + escapeHTML(compactError(err)) + '</div>');
  }
}

function renderPullRequestCreateDiff(html) {
  const target = document.querySelector('[data-pr-create-diff]');
  if (target) target.innerHTML = html;
}

async function handlePullRequestCreate(form) {
  const source = form.elements.source?.value || '';
  const target = form.elements.target?.value || '';
  if (!source || !target || source === target) {
    updatePullRequestCreateSummary();
    return;
  }
  const button = form.querySelector('button[type="submit"]');
  if (button) button.disabled = true;
  try {
    const data = await postJSON('/api/actions/pr', {
      action: 'create',
      source,
      target,
      title: formValue(form, 'title'),
      body: formValue(form, 'body')
    });
    const id = Number(data?.pr?.id || 0);
    window.location.href = id ? '/prs/' + id : '/prs';
  } catch (err) {
    setSyncStatus(compactError(err), 'is-stale');
  } finally {
    if (button) button.disabled = false;
  }
}

function shortRefName(ref) {
  return String(ref || '').replace(/^refs\/heads\//, '').replace(/^refs\/tags\//, '');
}

function showPullRequestReplyEditor(trigger) {
  const panel = trigger.closest('[data-pr-id]');
  if (!panel) return;
  const host = trigger.closest('.pr-inline-comment-body') || trigger.closest('.pr-reply') || trigger.closest('.pr-note');
  if (!host) return;
  const existing = host.parentElement ? host.parentElement.querySelector('[data-draft-editor][data-draft-kind="reply"]') : null;
  if (existing) existing.remove();
  const editor = document.createElement('div');
  editor.className = 'inline-draft-editor';
  if (host.parentElement && host.parentElement.classList.contains('pr-inline-after-context')) {
    editor.classList.add('pr-inline-reply-editor');
  }
  editor.setAttribute('data-draft-editor', '');
  editor.setAttribute('data-draft-kind', 'reply');
  editor.setAttribute('data-pr-id', panel.getAttribute('data-pr-id') || '');
  editor.setAttribute('data-target-note-id', trigger.getAttribute('data-target-note-id') || '');
  editor.setAttribute('data-target-comment-id', trigger.getAttribute('data-target-comment-id') || '');
  editor.innerHTML = inlineDraftEditorHTML('Reply');
  host.insertAdjacentElement('afterend', editor);
  focusInlineDraft(editor);
}

async function submitPullRequestReply(editor) {
  const id = Number(editor.getAttribute('data-pr-id') || 0);
  const textarea = editor.querySelector('[data-draft-text]');
  const text = textarea ? textarea.value.trim() : '';
  if (!text) return;
  const targetNoteID = Number(editor.getAttribute('data-target-note-id') || 0);
  const targetCommentID = Number(editor.getAttribute('data-target-comment-id') || 0);
  try {
    setInlineDraftBusy(editor, true);
    const data = await postJSON('/api/actions/pr', {
      id,
      action: 'reply',
      comment: text,
      target_note_id: targetNoteID,
      target_comment_id: targetCommentID
    });
    if (Array.isArray(data.prs)) updatePullRequestUI(data.prs);
    rememberPullRequestScrollTarget(findSubmittedReplyTarget(data, id, text) || fallbackReplyTarget(targetNoteID, targetCommentID));
    setSyncStatus('Reply added.', 'is-current');
    window.location.reload();
  } catch (err) {
    setSyncStatus(compactError(err), 'is-stale');
  } finally {
    setInlineDraftBusy(editor, false);
  }
}

function fallbackReplyTarget(noteID, commentID) {
  if (commentID) return 'pr-comment-' + commentID;
  if (noteID) return 'pr-note-' + noteID;
  return '';
}

function rememberPullRequestScrollTarget(targetID) {
  if (!targetID) return;
  try {
    window.sessionStorage.setItem(prScrollTargetKey, targetID);
  } catch (_) {}
}

function restorePullRequestScrollTarget() {
  let targetID = '';
  try {
    targetID = window.sessionStorage.getItem(prScrollTargetKey) || '';
    if (targetID) window.sessionStorage.removeItem(prScrollTargetKey);
  } catch (_) {}
  if (!targetID) return;
  window.setTimeout(function () {
    const target = document.getElementById(targetID);
    if (!target) return;
    target.scrollIntoView({block: 'center', behavior: 'smooth'});
    target.classList.add('is-scroll-target');
    window.setTimeout(function () { target.classList.remove('is-scroll-target'); }, 1800);
  }, 80);
}

function findSubmittedReplyTarget(data, prID, body) {
  const prs = Array.isArray(data && data.prs) ? data.prs : [];
  const pr = prs.find(function (item) { return Number(item.id || 0) === Number(prID || 0); });
  if (!pr) return '';
  const normalizedBody = String(body || '').trim();
  let found = null;
  for (const note of [].concat(pr.comments || [], pr.reviews || [])) {
    for (const comment of collectPullRequestComments(note)) {
      if (String(comment.body || '').trim() === normalizedBody) {
        if (!found || Number(comment.id || 0) > Number(found.id || 0)) found = comment;
      }
    }
  }
  return found && found.id ? 'pr-comment-' + found.id : '';
}

function collectPullRequestComments(noteOrComment) {
  const out = [];
  function visit(comment) {
    if (!comment) return;
    out.push(comment);
    for (const reply of comment.replies || []) visit(reply);
  }
  for (const comment of noteOrComment.comments || []) visit(comment);
  for (const reply of noteOrComment.replies || []) visit(reply);
  return out;
}

function setPullRequestActionsBusy(panel, busy, activeAction) {
  for (const button of panel.querySelectorAll('[data-pr-action]')) {
    button.disabled = busy;
    if (!button.dataset.label) button.dataset.label = button.textContent;
    if (busy && button.getAttribute('data-pr-action') === activeAction) {
      button.textContent = 'Working...';
    } else {
      button.textContent = button.dataset.label;
    }
  }
}

function showReviewDraftEditor(button) {
  const filePanel = button.closest('[data-review-file]');
  const row = button.closest('.visual-diff-row');
  const host = row || filePanel;
  if (!host) return;
  const existing = host.parentElement ? host.parentElement.querySelector('[data-draft-editor][data-draft-kind="review-comment"]') : null;
  if (existing) existing.remove();
  const editor = document.createElement('div');
  editor.className = row ? 'visual-diff-row review-draft-editor-row' : 'inline-draft-editor';
  editor.setAttribute('data-draft-editor', '');
  editor.setAttribute('data-draft-kind', 'review-comment');
  const target = reviewDraftTargetFromButton(button);
  editor.setAttribute('data-review-kind', target.kind);
  editor.setAttribute('data-review-file', target.file);
  editor.setAttribute('data-review-line', String(target.line || 0));
  editor.innerHTML = row ? '<div></div><pre></pre><div></div><pre>' + inlineDraftEditorHTML('Comment') + '</pre>' : inlineDraftEditorHTML('Comment');
  if (row) row.insertAdjacentElement('afterend', editor);
  else filePanel.querySelector('.diff-header')?.insertAdjacentElement('afterend', editor);
  editor._reviewTrigger = button;
  focusInlineDraft(editor);
  if (!restoringReviewDraft) saveReviewDraftState();
}

function addReviewDraftComment(button, text) {
  const comment = reviewCommentFromButton(button, text);
  reviewDraftComments.push(comment);
  renderReviewDraftComment(button, comment);
  updateReviewDraftState();
  saveReviewDraftState();
}

function reviewCommentFromButton(button, text) {
  const filePanel = button.closest('[data-review-file]');
  const row = button.closest('.visual-diff-row');
  const file = button.getAttribute('data-file') || (filePanel ? filePanel.getAttribute('data-review-file') : '');
  const line = Number(button.getAttribute('data-line') || (row ? row.querySelector('[data-new-line]')?.getAttribute('data-new-line') : 0) || 0);
  return {
    body: text,
    file,
    kind: button.hasAttribute('data-review-comment-file') ? 'file' : 'line',
    side: 'new',
    hunk: row ? row.getAttribute('data-hunk') || '' : '',
    hunk_index: Number(row ? row.getAttribute('data-hunk-index') || 0 : 0),
    old_start: Number(row ? row.getAttribute('data-old-start') || 0 : 0),
    new_start: Number(row ? row.getAttribute('data-new-start') || 0 : 0),
    offset: Number(row ? row.getAttribute('data-offset') || 0 : 0),
    line,
    line_text: row ? (row.querySelector('pre[data-new-line]')?.innerText || '').replace(/💬\s*$/, '').trimEnd() : ''
  };
}

function submitInlineDraft(button) {
  const editor = button.closest('[data-draft-editor]');
  if (!editor) return;
  const textarea = editor.querySelector('[data-draft-text]');
  const text = textarea ? textarea.value.trim() : '';
  if (!text) {
    editor.classList.add('has-error');
    if (textarea) textarea.focus();
    return;
  }
  editor.classList.remove('has-error');
  const kind = editor.getAttribute('data-draft-kind') || '';
  if (kind === 'reply') {
    submitPullRequestReply(editor);
    return;
  }
  if (kind === 'review-comment') {
    const trigger = editor._reviewTrigger || findReviewDraftButton({
      kind: editor.getAttribute('data-review-kind') || 'line',
      file: editor.getAttribute('data-review-file') || '',
      line: Number(editor.getAttribute('data-review-line') || 0)
    });
    if (!trigger) return;
    addReviewDraftComment(trigger, text);
    editor.remove();
    saveReviewDraftState();
  }
}

function inlineDraftEditorHTML(label) {
  return '<div class="inline-draft-box"><textarea data-draft-text rows="4" placeholder="' + escapeHTML(label || 'Comment') + '"></textarea><div class="inline-draft-actions"><button type="button" class="button-link primary" data-draft-ok>OK</button><button type="button" class="button-link" data-draft-cancel>Cancel</button></div><div class="inline-draft-error">Comment is required.</div></div>';
}

function focusInlineDraft(editor) {
  window.setTimeout(function () {
    const textarea = editor.querySelector('[data-draft-text]');
    if (textarea) textarea.focus();
  }, 0);
}

function setInlineDraftBusy(editor, busy) {
  for (const button of editor.querySelectorAll('button')) button.disabled = busy;
  const textarea = editor.querySelector('[data-draft-text]');
  if (textarea) textarea.disabled = busy;
}

function renderReviewDraftComment(button, comment) {
  const row = button.closest('.visual-diff-row');
  const host = row || button.closest('[data-review-file]');
  if (!host) return;
  const html = '<div class="review-draft-comment"><div class="pr-reply-meta"><strong>You</strong> commented' + (comment.kind === 'line' && comment.line ? ' <span>line ' + escapeHTML(comment.line) + '</span>' : '') + '</div><div>' + escapeHTML(comment.body) + '</div></div>';
  if (row) row.insertAdjacentHTML('afterend', '<div class="visual-diff-row review-draft-row"><div></div><pre></pre><div></div><pre>' + html + '</pre></div>');
  else host.querySelector('.diff-header')?.insertAdjacentHTML('afterend', html);
}

function updateReviewDraftState() {
  const form = document.querySelector('[data-pr-review-submit]');
  if (!form) return;
  form.classList.toggle('has-drafts', reviewDraftComments.length > 0);
}

async function submitReviewDraft(button) {
  const form = button.closest('[data-pr-review-submit]');
  if (!form) return;
  const id = Number(form.getAttribute('data-pr-id') || 0);
  const note = form.querySelector('[data-pr-review-note]');
  const action = button.getAttribute('data-pr-review-action') || 'comment';
  const mapped = action === 'approve' ? 'approve' : action === 'reject' ? 'reject' : 'review-comment';
  if (!reviewDraftComments.length && !String(note ? note.value : '').trim() && mapped === 'review-comment') {
    setSyncStatus('Add a review note or at least one inline comment.', 'is-stale');
    return;
  }
  button.disabled = true;
  try {
    const data = await postJSON('/api/actions/pr', {
      id,
      action: mapped,
      comment: note ? note.value.trim() : '',
      comments: reviewDraftComments
    });
    reviewDraftComments.splice(0, reviewDraftComments.length);
    clearStoredReviewDraft(id);
    updatePullRequestUI(data.prs || []);
    window.location.href = '/prs/' + encodeURIComponent(String(id));
  } catch (err) {
    setSyncStatus(compactError(err), 'is-stale');
  } finally {
    button.disabled = false;
  }
}

function currentReviewID() {
  const form = document.querySelector('[data-pr-review-submit]');
  if (form) return Number(form.getAttribute('data-pr-id') || 0);
  const review = document.querySelector('[data-pr-review-diff]');
  return review ? Number(review.getAttribute('data-pr-id') || 0) : 0;
}

function reviewDraftStorageKey(id) {
  return id ? 'bgit.reviewDraft.' + id : '';
}

function saveReviewDraftState() {
  if (restoringReviewDraft) return;
  const id = currentReviewID();
  const key = reviewDraftStorageKey(id);
  if (!key) return;
  const note = document.querySelector('[data-pr-review-note]');
  const editors = Array.from(document.querySelectorAll('[data-draft-editor][data-draft-kind="review-comment"]')).map(function (editor) {
    const textarea = editor.querySelector('[data-draft-text]');
    return {
      kind: editor.getAttribute('data-review-kind') || 'line',
      file: editor.getAttribute('data-review-file') || '',
      line: Number(editor.getAttribute('data-review-line') || 0),
      text: textarea ? textarea.value : ''
    };
  }).filter(function (editor) { return editor.file || editor.text; });
  const state = {
    note: note ? note.value : '',
    comments: reviewDraftComments,
    editors
  };
  if (!state.note && !state.comments.length && !state.editors.length) {
    window.localStorage.removeItem(key);
    return;
  }
  window.localStorage.setItem(key, JSON.stringify(state));
}

function restoreReviewDraftState() {
  const id = currentReviewID();
  const key = reviewDraftStorageKey(id);
  if (!key) return;
  let state = null;
  try {
    state = JSON.parse(window.localStorage.getItem(key) || 'null');
  } catch (_) {
    state = null;
  }
  if (!state) return;
  restoringReviewDraft = true;
  const note = document.querySelector('[data-pr-review-note]');
  if (note && typeof state.note === 'string') note.value = state.note;
  for (const comment of Array.isArray(state.comments) ? state.comments : []) {
    const button = findReviewDraftButton(comment);
    if (!button) continue;
    reviewDraftComments.push(comment);
    renderReviewDraftComment(button, comment);
  }
  for (const draft of Array.isArray(state.editors) ? state.editors : []) {
    const button = findReviewDraftButton(draft);
    if (!button) continue;
    showReviewDraftEditor(button);
    const editor = document.querySelector('[data-draft-editor][data-review-file="' + cssEscape(draft.file || '') + '"][data-review-line="' + String(Number(draft.line || 0)) + '"]');
    const textarea = editor ? editor.querySelector('[data-draft-text]') : null;
    if (textarea) textarea.value = draft.text || '';
  }
  restoringReviewDraft = false;
  updateReviewDraftState();
  saveReviewDraftState();
}

function clearStoredReviewDraft(id) {
  const key = reviewDraftStorageKey(id);
  if (key) window.localStorage.removeItem(key);
}

function reviewDraftTargetFromButton(button) {
  const filePanel = button.closest('[data-review-file]');
  return {
    kind: button.hasAttribute('data-review-comment-file') ? 'file' : 'line',
    file: button.getAttribute('data-file') || (filePanel ? filePanel.getAttribute('data-review-file') : ''),
    line: Number(button.getAttribute('data-line') || 0)
  };
}

function findReviewDraftButton(target) {
  const file = target.file || '';
  const line = Number(target.line || 0);
  if ((target.kind || '') === 'file') {
    return document.querySelector('[data-review-comment-file="' + cssEscape(file) + '"]');
  }
  return document.querySelector('[data-review-comment-line][data-file="' + cssEscape(file) + '"][data-line="' + String(line) + '"]');
}

function cssEscape(value) {
  if (window.CSS && window.CSS.escape) return window.CSS.escape(String(value));
  return String(value).replace(/["\\]/g, '\\$&');
}

function modalDialog(options) {
  return new Promise(function (resolve) {
    const overlay = document.createElement('div');
    overlay.className = 'modal-overlay';
    const files = Array.isArray(options.files) ? options.files : [];
    const fileListHTML = files.length ? '<div class="modal-file-list"><div>Files to commit</div><ul>' + files.map(function (file) { return '<li>' + escapeHTML(file) + '</li>'; }).join('') + '</ul></div>' : '';
    const placeholder = options.placeholder ? ' placeholder="' + escapeHTML(options.placeholder) + '"' : '';
    const fieldHTML = options.multiline ? '<textarea data-modal-input rows="' + escapeHTML(String(options.rows || 5)) + '" autocomplete="off"' + placeholder + '></textarea>' : '<input type="text" data-modal-input autocomplete="off"' + placeholder + '>';
    const inputHTML = options.prompt ? '<label class="modal-field"><span>' + escapeHTML(options.inputLabel || 'Value') + '</span>' + fieldHTML + '</label><div class="modal-error" data-modal-error hidden></div>' : '';
    const titleHTML = options.title ? '<h2>' + escapeHTML(options.title || '') + '</h2>' : '';
    const bodyHTML = options.body ? '<p>' + escapeHTML(options.body || '') + '</p>' : '';
    const cardClass = 'modal-card' + (options.cardClass ? ' ' + escapeHTML(options.cardClass) : '');
    overlay.innerHTML = '<div class="' + cardClass + '" role="dialog" aria-modal="true">' + titleHTML + bodyHTML + fileListHTML + inputHTML + '<div class="modal-actions"><button type="button" class="button-link" data-modal-cancel>Cancel</button><button type="button" class="button-link primary" data-modal-confirm>' + escapeHTML(options.confirm || 'OK') + '</button></div></div>';
    document.body.appendChild(overlay);
    const input = overlay.querySelector('[data-modal-input]');
    const error = overlay.querySelector('[data-modal-error]');
    if (input && Object.prototype.hasOwnProperty.call(options, 'value')) {
      input.value = String(options.value || '');
    }
    const close = function (value) {
      overlay.remove();
      resolve(value);
    };
    overlay.querySelector('[data-modal-cancel]').addEventListener('click', function () { close(false); });
    overlay.querySelector('[data-modal-confirm]').addEventListener('click', function () {
      if (!input) {
        close(true);
        return;
      }
      const value = input.value.trim();
      if (options.required && !value) {
        if (error) {
          error.textContent = (options.inputLabel || 'Value') + ' is required.';
          error.hidden = false;
        }
        input.focus();
        return;
      }
      close(value);
    });
    overlay.addEventListener('click', function (event) {
      if (event.target === overlay) close(false);
    });
    overlay.addEventListener('keydown', function (event) {
      if (event.key === 'Escape') close(false);
      if (event.key === 'Enter' && (event.metaKey || event.ctrlKey)) {
        overlay.querySelector('[data-modal-confirm]').click();
      }
    });
    window.setTimeout(function () {
      (input || overlay.querySelector('[data-modal-confirm]')).focus();
    }, 0);
  });
}

async function hydrateRefs() {
  const select = document.querySelector('[data-ref-selector]');
  if (!select) return;
  const current = new URL(window.location.href).searchParams.get('ref') || select.value;
  try {
    const data = await fetchJSON('/api/refs');
    if (!Array.isArray(data.refs) || data.refs.length === 0) return;
    select.textContent = '';
    let currentGroup = '';
    let group = null;
    for (const ref of data.refs) {
      if (ref.kind !== currentGroup) {
        currentGroup = ref.kind;
        group = document.createElement('optgroup');
        group.label = currentGroup;
        select.appendChild(group);
      }
      const option = document.createElement('option');
      option.value = ref.full_name;
      option.textContent = ref.name;
      if (ref.full_name === current) option.selected = true;
      group.appendChild(option);
    }
  } catch (_) {
    // Server-rendered options remain usable if the JSON API is unavailable.
  }
}

async function refreshRemoteState(options) {
  options = options || {};
  if (remoteRefreshInFlight) return;
  remoteRefreshInFlight = true;
  setRemoteRefreshSpinning(true);
  if (!remoteSyncInitialized) {
    setRemoteSyncStatus('syncing', 'Synchronising');
  }
  try {
    const ref = currentSelectedRef();
    const data = await fetchJSON('/api/state' + (ref ? '?ref=' + encodeURIComponent(ref) : ''));
    currentWebState = data;
    applyRepositoryState(data);
    await refreshPullRequests(!!options.refreshPullRequests);
    reconcileRemoteState(data);
  } catch (err) {
    remoteSyncInitialized = true;
    setRemoteSyncStatus('error', compactError(err));
  } finally {
    remoteRefreshInFlight = false;
    setRemoteRefreshSpinning(false);
  }
}

async function refreshPullRequests(refresh) {
  const tab = document.querySelector('[data-pr-tab]');
  const list = document.querySelector('[data-pr-list]');
  if (!tab && !list) return;
  try {
    const data = await fetchJSON('/api/prs' + (refresh ? '?refresh=1' : ''));
    updatePullRequestUI(Array.isArray(data.prs) ? data.prs : []);
  } catch (_) {
    // Lack of PR visibility should not affect repository freshness.
  }
}

function currentSelectedRef() {
  const urlRef = new URL(window.location.href).searchParams.get('ref');
  if (urlRef) return urlRef;
  const selector = document.querySelector('[data-ref-selector]');
  return selector ? selector.value : '';
}

function updatePullRequestUI(prs) {
  const tab = document.querySelector('[data-pr-tab]');
  if (tab) tab.hidden = prs.length === 0;
  const count = document.querySelector('[data-pr-tab-count]');
  if (count) {
    count.textContent = String(openPullRequestCount(prs));
  }
  const list = document.querySelector('[data-pr-list]');
  if (list) {
    list.innerHTML = pullRequestListHTML(prs);
  }
}

function openPullRequestCount(prs) {
  return prs.filter(function (pr) {
    return String((pr && pr.status) || 'open').toLowerCase() === 'open';
  }).length;
}

function pullRequestListHTML(prs) {
  if (!prs.length) return '<div class="empty">No pull requests found.</div>';
  return '<ul class="pr-list">' + prs.map(function (pr) {
    const approvals = Number(pr.approvals || 0);
    const approvalText = approvals > 0 ? '<span class="pr-approvals">' + approvals + ' approval' + (approvals === 1 ? '' : 's') + '</span>' : '';
    const id = escapeHTML(String(pr.id || ''));
    const url = '/prs/' + id;
    return '<li class="pr-item" data-pr-href="' + url + '"><div class="pr-main"><div><a class="pr-title" href="' + url + '"><span class="pr-id">#' + id + '</span> <strong>' + escapeHTML(pr.title || 'Untitled pull request') + '</strong></a></div><div class="muted">' + escapeHTML(shortRefName(pr.source || '')) + ' → ' + escapeHTML(shortRefName(pr.target || '')) + '</div></div><div class="pr-meta"><span class="pr-status">' + escapeHTML(pr.status || 'open') + '</span>' + approvalText + '</div></li>';
  }).join('') + '</ul>';
}

function shortRefName(ref) {
  return String(ref || '').replace(/^refs\/heads\//, '').replace(/^refs\/tags\//, '');
}

function escapeHTML(value) {
  return String(value).replace(/[&<>"']/g, function (ch) {
    return {'&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;'}[ch];
  });
}

function reconcileRemoteState(data) {
  remoteSyncInitialized = true;
  if (data && data.fetch_error) {
    setRemoteSyncStatus('error', compactError({message: data.fetch_error}));
    return;
  }
  if (data && Number(data.behind || 0) > 0) {
    setRemoteSyncStatus('behind', 'NOT PULLED');
    return;
  }
  if (data && Number(data.ahead || 0) > 0) {
    setRemoteSyncStatus('ahead', 'NOT PUSHED');
    return;
  }
  if (data && Array.isArray(data.unstaged_files) && data.unstaged_files.length > 0) {
    setRemoteSyncStatus('dirty', 'UNSTAGED');
    return;
  }
  if (data && Array.isArray(data.untracked_files) && data.untracked_files.length > 0) {
    setRemoteSyncStatus('dirty', 'UNTRACKED');
    return;
  }
  if (data && data.dirty) {
    setRemoteSyncStatus('dirty', 'UNCOMMITTED');
    return;
  }
  setRemoteSyncStatus('current', 'SYNCHED');
}

function applyRepositoryState(state) {
  clearStateMarkers();
  if (!state) return;
  const staged = new Set((state.staged_files || []).map(pathKey));
  const unstaged = new Set((state.unstaged_files || []).map(pathKey));
  const untracked = new Set((state.untracked_files || []).map(pathKey));
  const unpushed = new Set((state.unpushed_files || []).map(pathKey));
  const unpulled = new Set((state.unpulled_files || []).map(pathKey));
  for (const row of document.querySelectorAll('[data-file-row]')) {
    const path = pathKey(row.getAttribute('data-file-path') || '');
    if (!path || path === '..') continue;
    if (matchesStatePath(path, untracked)) addFileState(row, 'UNTRACKED', 'dirty', 'stage', path);
    if (matchesStatePath(path, unstaged)) addFileState(row, 'UNSTAGED', 'dirty', 'stage', path);
    if (matchesStatePath(path, staged)) addFileState(row, 'UNCOMMITTED', 'dirty', 'unstage', path);
    if (matchesStatePath(path, unpushed)) addFileState(row, 'NOT PUSHED', 'ahead');
    if (matchesStatePath(path, unpulled)) addFileState(row, 'NOT PULLED', 'behind');
  }
  addSyntheticFileRows(untracked, 'UNTRACKED', 'dirty');
  addSyntheticFileRows(staged, 'UNCOMMITTED', 'dirty');
  updateRepoActionButtons(state);
  markCommits(state.unpushed_commits || [], 'NOT PUSHED', 'ahead');
  markCommits(state.unpulled_commits || [], 'NOT PULLED', 'behind');
}

function clearStateMarkers() {
  for (const el of document.querySelectorAll('[data-state-marker]')) el.remove();
  for (const row of document.querySelectorAll('.is-state-dirty,.is-state-ahead,.is-state-behind')) {
    row.classList.remove('is-state-dirty', 'is-state-ahead', 'is-state-behind');
  }
  updateRepoActionButtons(null);
}

function pathKey(path) {
  return String(path || '').replace(/^\/+/, '');
}

function matchesStatePath(rowPath, statePaths) {
  if (statePaths.has(rowPath)) return true;
  return Array.from(statePaths).some(function (path) {
    return path.startsWith(rowPath + '/');
  });
}

function addSyntheticFileRows(paths, label, kind) {
  const table = document.querySelector('[data-file-list]');
  if (!table) return;
  const current = currentTreePath();
  const existing = new Set(Array.from(document.querySelectorAll('[data-file-row]')).map(function (row) {
    return pathKey(row.getAttribute('data-file-path') || '');
  }));
  for (const path of paths) {
    if (!path || existing.has(path)) continue;
    const parent = path.includes('/') ? path.slice(0, path.lastIndexOf('/')) : '';
    if (parent !== current) continue;
    const name = path.includes('/') ? path.slice(path.lastIndexOf('/') + 1) : path;
    const row = document.createElement('tr');
    row.className = 'is-state-' + kind;
    row.setAttribute('data-state-marker', 'true');
    row.setAttribute('data-file-row', '');
    row.setAttribute('data-file-name', name.toLowerCase());
    row.setAttribute('data-file-path', path);
    row.innerHTML = '<td class="kind">file</td><td><span>' + escapeHTML(name) + '</span><span class="state-actions" data-file-state>' + stateMarkerHTML(label, kind, stateActionForKind(kind, path)) + '</span></td><td class="hash">local</td>';
    table.appendChild(row);
  }
}

function currentTreePath() {
  const path = window.location.pathname;
  if (!path.startsWith('/tree/')) return '';
  return pathKey(decodeURIComponent(path.slice('/tree/'.length)));
}

function addFileState(row, label, kind, actionKind, path) {
  row.classList.add('is-state-' + kind);
  const target = row.querySelector('[data-file-state]') || row.children[1];
  if (!target) return;
  const actions = actionKind ? stateActionForKind(actionKind, path) : stateActionForKind(kind);
  target.insertAdjacentHTML('beforeend', stateMarkerHTML(label, kind, actions));
}

function markCommits(commits, label, kind) {
  const hashes = new Map();
  for (const commit of commits) {
    if (commit.hash) hashes.set(commit.hash, commit);
  }
  for (const row of document.querySelectorAll('[data-commit-row]')) {
    const hash = row.getAttribute('data-commit-hash') || '';
    if (!hashes.has(hash)) continue;
    row.classList.add('is-state-' + kind);
    const target = row.querySelector('[data-commit-state]') || row.firstElementChild;
    if (target) target.insertAdjacentHTML('beforeend', stateMarkerHTML(label, kind, ''));
    hashes.delete(hash);
  }
  const list = document.querySelector('.commits');
  if (!list || kind !== 'behind') return;
  const missing = Array.from(hashes.values()).reverse();
  for (const commit of missing) {
    const li = document.createElement('li');
    li.className = 'is-state-behind';
    li.setAttribute('data-state-marker', 'true');
    const commitURL = '/commits?commit=' + encodeURIComponent(commit.hash || '');
    li.setAttribute('data-commit-row', 'true');
    li.setAttribute('data-commit-hash', commit.hash || '');
    li.setAttribute('data-commit-href', commitURL);
    li.innerHTML = '<div class="commit-row-main"><a class="commit-subject" href="' + commitURL + '">' + escapeHTML(commit.subject || commit.short_hash || '') + '</a>' + stateMarkerHTML(label, kind, '') + '<div class="meta">' + escapeHTML(commit.author || '') + ' authored remotely</div></div><div class="commit-row-meta"><a class="commit-hash-link" href="' + commitURL + '"><code>' + escapeHTML(commit.short_hash || '') + '</code></a></div>';
    list.insertBefore(li, list.firstChild);
  }
}

function stateActionForKind(kind, path) {
  if (kind === 'stage') return diffActionHTML(path, 'worktree') + '<button type="button" class="inline-action" data-web-action="discard" data-path="' + escapeHTML(path || '') + '">CHECKOUT</button><button type="button" class="inline-action" data-web-action="stage" data-path="' + escapeHTML(path || '') + '">STAGE</button>';
  if (kind === 'unstage') return diffActionHTML(path, 'staged') + '<button type="button" class="inline-action" data-web-action="unstage" data-path="' + escapeHTML(path || '') + '">UNSTAGE</button>';
  if (kind === 'commit') return '<button type="button" class="inline-action" data-web-action="commit">Commit</button>';
  return '';
}

function diffActionHTML(path, mode) {
  return '<button type="button" class="inline-icon-action" data-web-diff data-path="' + escapeHTML(path || '') + '" data-mode="' + escapeHTML(mode || 'worktree') + '" title="View diff" aria-label="View diff">' + diffIconSVG() + '</button>';
}

function diffIconSVG() {
  return '<svg viewBox="0 0 24 24" fill="none" aria-hidden="true" focusable="false"><path opacity="0.1" d="M9 6C9 7.65685 7.65685 9 6 9C4.34315 9 3 7.65685 3 6C3 4.34315 4.34315 3 6 3C7.65685 3 9 4.34315 9 6Z" fill="currentColor"/><path opacity="0.1" d="M21 18C21 19.6569 19.6569 21 18 21C16.3431 21 15 19.6569 15 18C15 16.3431 16.3431 15 18 15C19.6569 15 21 16.3431 21 18Z" fill="currentColor"/><path d="M9 6C9 7.65685 7.65685 9 6 9C4.34315 9 3 7.65685 3 6C3 4.34315 4.34315 3 6 3C7.65685 3 9 4.34315 9 6Z" stroke="currentColor" stroke-width="2"/><path d="M21 18C21 19.6569 19.6569 21 18 21C16.3431 21 15 19.6569 15 18C15 16.3431 16.3431 15 18 15C19.6569 15 21 16.3431 21 18Z" stroke="currentColor" stroke-width="2"/><path d="M15 3L12.0605 5.93945C12.0271 5.97289 12.0271 6.02711 12.0605 6.06055L15 9" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"/><path d="M9 21L11.9473 18.0527C11.9764 18.0236 11.9764 17.9764 11.9473 17.9473L9 15" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"/><path d="M12 6C14.8284 6 16.2426 6 17.1213 6.87868C18 7.75736 18 9.17157 18 12V15" stroke="currentColor" stroke-width="2"/><path d="M12 18C9.17157 18 7.75736 18 6.87868 17.1213C6 16.2426 6 14.8284 6 12L6 9" stroke="currentColor" stroke-width="2"/></svg>';
}

function updateRepoActionButtons(state) {
  const control = document.querySelector('.repo-action-control');
  if (!control || control.getAttribute('data-code-actions') !== 'true') {
    setRepoActionButton('[data-repo-commit]', 0, 'COMMIT');
    setRepoActionButton('[data-repo-push]', 0, 'PUSH');
    setRepoActionButton('[data-repo-pull]', 0, 'PULL');
    setRepoActionButton('[data-repo-uncommit]', 0, 'UNCOMMIT');
    return;
  }
  const stagedCount = state && Array.isArray(state.staged_files) ? state.staged_files.length : 0;
  const aheadCount = state ? Number(state.ahead || 0) : 0;
  const behindCount = state ? Number(state.behind || 0) : 0;
  setRepoActionButton('[data-repo-commit]', stagedCount, 'COMMIT');
  setRepoActionButton('[data-repo-push]', aheadCount, 'PUSH');
  setRepoActionButton('[data-repo-pull]', behindCount, 'PULL');
  setRepoActionButton('[data-repo-uncommit]', aheadCount, 'UNCOMMIT');
  applyCapabilityUI();
}

function setRepoActionButton(selector, count, label) {
  const button = document.querySelector(selector);
  if (!button) return;
  button.hidden = Number(count || 0) <= 0;
  button.dataset.actionLabel = label;
  button.textContent = label;
}

function setWebActionsBusy(busy, activeLabel) {
  webActionInFlight = busy;
  for (const button of document.querySelectorAll('[data-web-action]')) {
    button.disabled = busy;
    if (button.matches('.repo-action-button')) {
      if (busy && activeLabel && !button.hidden) {
        button.textContent = activeLabel;
      } else if (!busy && button.dataset.actionLabel) {
        button.textContent = button.dataset.actionLabel;
      }
    }
  }
  applyCapabilityUI();
}

function stateMarkerHTML(label, kind, action) {
  return '<span class="state-marker state-' + kind + '" data-state-marker><span>' + escapeHTML(label) + '</span>' + (action || '') + '</span>';
}

function setRemoteSyncStatus(state, text) {
  const badge = document.querySelector('[data-remote-sync-badge]');
  const button = document.querySelector('[data-remote-refresh]');
  if (!badge || !button) return;
  badge.textContent = text;
  badge.title = text;
  badge.className = 'remote-badge is-' + state;
  const syncing = state === 'syncing';
  button.disabled = syncing;
  button.classList.toggle('is-spinning', syncing);
  button.classList.toggle('is-current', state === 'current');
}

function setRemoteRefreshSpinning(spinning) {
  const button = document.querySelector('[data-remote-refresh]');
  if (!button) return;
  button.disabled = spinning;
  button.classList.toggle('is-spinning', spinning);
  if (spinning) button.classList.remove('is-current');
}

function compactError(err) {
  let text = err && err.message ? err.message : 'Remote check failed';
  text = text.replace(/\s+/g, ' ').trim();
  if (!text) text = 'Remote check failed';
  if (text.length > 80) text = text.slice(0, 77) + '...';
  return text;
}

function reloadLocalView() {
  const url = new URL(window.location.href);
  url.searchParams.delete('_remote');
  url.searchParams.set('_ts', String(Date.now()));
  window.location.replace(url.toString());
}

function remoteHeadHash(data) {
  if (data.commit && data.commit.hash) return data.commit.hash;
  if (data.head && data.head.hash) return data.head.hash;
  if (Array.isArray(data.commits) && data.commits[0] && data.commits[0].hash) return data.commits[0].hash;
  return '';
}

function setSyncStatus(text, cls) {
  const el = document.querySelector('[data-sync-status]');
  if (!el) return;
  window.clearTimeout(setSyncStatus.timer);
  el.textContent = text;
  el.className = 'sync-status is-visible ' + (cls || '');
  if (cls === 'is-current') {
    setSyncStatus.timer = window.setTimeout(function () {
      el.classList.remove('is-visible');
    }, 1800);
  }
}

function clearSyncStatus() {
  const el = document.querySelector('[data-sync-status]');
  if (!el) return;
  window.clearTimeout(setSyncStatus.timer);
  el.classList.remove('is-visible');
}

function query(values) {
  const params = new URLSearchParams();
  for (const [key, value] of Object.entries(values)) {
    if (value) params.set(key, value);
  }
  const text = params.toString();
  return text ? '?' + text : '';
}
