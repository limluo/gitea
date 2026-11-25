import {test, expect} from '@playwright/test';
import {loginUser, randomString} from './utils.ts';

test('delivery workbench initializes repositories and tracks requirements', async ({page}) => {
  await loginUser(page, 'user1');
  await page.goto('/user2/repo1/delivery');
  await page.getByLabel('工作角色').selectOption('tech');
  await page.getByRole('navigation', {name: '业务导航'}).getByText('仓库管理').click();
  await page.getByRole('button', {name: '初始化仓库', exact: true}).click();
  const dialog = page.getByRole('dialog');
  const repoName = `business-${randomString(8)}`;
  await dialog.getByLabel('仓库名称').fill(repoName);
  await dialog.getByRole('button', {name: '初始化仓库'}).click();
  await expect(page).toHaveURL(new RegExp(`/${repoName}/delivery\\?tab=settings$`));
  await page.getByRole('link', {name: '需求与分支'}).click();
  await page.getByRole('button', {name: '新建需求分支'}).click();
  await dialog.getByLabel('需求名称').fill('订单导出');
  await dialog.getByLabel('验收标准').fill('包含订单日期');
  await dialog.getByLabel('分支名称').fill('feature/export');
  await dialog.getByRole('button', {name: '保存'}).click();
  await page.getByRole('button', {name: '分支详情'}).click();
  await expect(dialog).toContainText('包含订单日期');
  await dialog.getByRole('button', {name: '关闭'}).click();
  await page.getByRole('link', {name: '部署环境'}).click();
  await page.getByRole('button', {name: '配置部署'}).first().click();
  await expect(dialog.getByLabel('目标主机 / IP')).toBeVisible();
});
