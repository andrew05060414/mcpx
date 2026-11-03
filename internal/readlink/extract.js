async (currentPlatform) => {
  const text = (selector) => [...document.querySelectorAll(selector)].map((node) => node.innerText || node.textContent || '').join('\n').trim();
  const firstText = (selectors) => selectors.map((selector) => text(selector)).find(Boolean) || '';
  const meta = (name) => document.querySelector(`meta[name="${name}"], meta[property="${name}"]`)?.content || '';
  const state = window.__INITIAL_STATE__ || window.__NEXT_DATA__ || {};
  const video = state.videoData || state.video || state;
  let subtitles = '';
  if (currentPlatform === 'bilibili') {
    const tracks = [
      ...(video.subtitle?.list || []),
      ...(video.subtitle?.subtitles || []),
      ...(state.subtitle?.list || []),
    ].map((item) => item.subtitle_url || item.subtitleUrl || item.url).filter(Boolean);
    for (const rawTrack of [...new Set(tracks)].slice(0, 3)) {
      try {
        const response = await fetch(new URL(rawTrack, location.href), { credentials: 'include' });
        if (!response.ok) continue;
        const payload = await response.json();
        const rows = payload.body || payload.data?.body || payload.data || payload;
        if (Array.isArray(rows)) {
          subtitles = rows.map((row) => row.content || row.text || '').filter(Boolean).join('\n');
          if (subtitles) break;
        }
      } catch {
        // The page can still provide title/description/body when subtitle fetch is blocked.
      }
    }
  }
  const bodySelectors = {
    xiaohongshu: ['#detail-desc', '.note-scroller', '[class*="note-content"]', 'main'],
    bilibili: ['.video-desc', '#v_desc', '[class*="video-desc"]', 'main'],
    zhihu: ['.RichContent-inner', '.Post-RichTextContainer', 'article', 'main'],
    x: ['article', 'main'],
  };
  const authorSelectors = {
    xiaohongshu: ['[class*="author"]', '[class*="user"]'],
    bilibili: ['.up-name', '[class*="up-name"]'],
    zhihu: ['.AuthorInfo-name', '[class*="AuthorInfo"]'],
    x: ['[data-testid="User-Name"]'],
  };
  return {
    title: video.title || firstText(currentPlatform === 'xiaohongshu' ? ['#detail-title', 'h1'] : ['h1']) || meta('og:title') || document.title,
    description: video.desc || firstText(currentPlatform === 'bilibili' ? ['.video-desc', '#v_desc'] : ['meta[name="description"]']) || meta('og:description'),
    author: firstText(authorSelectors[currentPlatform] || []),
    body: firstText(bodySelectors[currentPlatform] || ['main', 'article']),
    subtitles,
    status: 200,
    url: location.href,
  };
}
