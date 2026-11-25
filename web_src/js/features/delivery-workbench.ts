export function initDeliveryWorkbench() {
  const shell = document.querySelector('.delivery-shell');
  if (!shell) return;
  for (const select of shell.querySelectorAll<HTMLSelectElement>('select[data-delivery-repo]')) {
    select.addEventListener('change', () => window.location.assign(select.value));
  }
  const persona = shell.querySelector<HTMLSelectElement>('select[data-delivery-persona]');
  persona?.addEventListener('change', () => {
    document.cookie = `delivery-view=${persona.value}; Path=/; SameSite=Lax`;
    window.location.assign('?tab=overview');
  });
  if (!('commandForElement' in HTMLButtonElement.prototype)) {
    for (const button of shell.querySelectorAll<HTMLButtonElement>('button[commandfor]')) {
      button.addEventListener('click', (event) => {
        event.preventDefault();
        const dialog = document.getElementById(button.getAttribute('commandfor')!);
        if (!(dialog instanceof HTMLDialogElement)) return;
        if (button.getAttribute('command') === 'close') dialog.close();
        else dialog.showModal();
      });
    }
  }
  shell.querySelector<HTMLDialogElement>('dialog[data-delivery-open]')?.showModal();
  for (const form of shell.querySelectorAll<HTMLFormElement>('form[data-review-form]')) {
    const select = form.querySelector<HTMLSelectElement>('select[name=op]');
    const expert = form.querySelector<HTMLInputElement>('input[name=expert]');
    const note = form.querySelector<HTMLTextAreaElement>('textarea[name=note]');
    if (!select || !expert || !note) continue;
    const update = () => {
      expert.required = select.value === 'expert';
      expert.disabled = !expert.required;
      expert.closest('label')!.hidden = expert.disabled;
      note.required = ['reject', 'revise'].includes(select.value);
    };
    select.addEventListener('change', update);
    update();
  }
  for (const button of shell.querySelectorAll<HTMLButtonElement>('[data-delivery-diff]')) {
    button.addEventListener('click', async () => {
      const parent = button.closest('dialog')!;
      const dialog = document.createElement('dialog');
      const title = document.createElement('h2');
      title.textContent = '代码差异';
      const diff = document.createElement('pre');
      diff.textContent = '正在加载代码差异…';
      const back = document.createElement('button');
      back.textContent = '返回并填写结论';
      back.className = 'ui button';
      dialog.append(title, diff, back);
      parent.parentElement!.append(dialog);
      parent.close();
      dialog.showModal();
      back.addEventListener('click', () => dialog.close());
      const controller = new AbortController();
      dialog.addEventListener('close', () => {
        controller.abort();
        dialog.remove();
        parent.showModal();
        button.focus();
      }, {once: true});
      try {
        const response = await fetch(button.dataset.deliveryDiff!, {signal: controller.signal});
        if (!response.ok) throw new Error(String(response.status));
        diff.textContent = await response.text() || '没有代码差异';
      } catch {
        diff.textContent = '无法读取代码差异，请返回后重试。';
      }
    });
  }
}
