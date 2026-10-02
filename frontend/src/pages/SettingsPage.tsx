import { useCallback, useEffect, useState, type FormEvent } from 'react';
import { AlertCircle, ArrowUpCircle, Bug, Check, ExternalLink, Github, KeyRound, Network, RefreshCw, RotateCcw, Save, Settings2, ShieldAlert, Trash2 } from 'lucide-react';
import { Button, Chip, Input, Modal, ModalBody, ModalContent, ModalFooter, ModalHeader, Skeleton, Switch, Textarea, Tooltip, useDisclosure } from '@heroui/react';
import { PageShell, StatusDot } from '../components/PageShell';
import { useToast } from '../components/Toast';
import { apiFetch, type AuditCleanupResponse, type SettingsResponse, type UserAgentCheckResponse, type VersionInfo, errorMessage, formatDateTime, jsonRequest } from '../api';

type SettingsForm = Pick<SettingsResponse, 'user_agent' | 'platform_base_url' | 'codingplan_api_url' | 'gateway_url' | 'signer_url' | 'audit_debug_enabled' | 'system_prompt_enabled' | 'system_prompt' | 'request_timeout_seconds' | 'request_retry_count' | 'request_retry_status_codes' | 'audit_retention_days' | 'audit_detail_retention_days'>;
type ProjectVersionInfo = VersionInfo & { repository_url: string };
type AuditCleanupTarget = 'records' | 'details';

const emptyForm: SettingsForm = {
  user_agent: '', platform_base_url: '', codingplan_api_url: '', gateway_url: '', signer_url: '', audit_debug_enabled: false, system_prompt_enabled: false, system_prompt: '', request_timeout_seconds: 120, request_retry_count: 0, request_retry_status_codes: '', audit_retention_days: 30, audit_detail_retention_days: 30,
};

function settingsForm(response: SettingsResponse): SettingsForm {
  return {
    user_agent: response.user_agent,
    platform_base_url: response.platform_base_url,
    codingplan_api_url: response.codingplan_api_url,
    gateway_url: response.gateway_url,
    signer_url: response.signer_url,
    audit_debug_enabled: response.audit_debug_enabled,
    system_prompt_enabled: response.system_prompt_enabled,
    system_prompt: response.system_prompt,
    request_timeout_seconds: response.request_timeout_seconds,
    request_retry_count: response.request_retry_count,
    request_retry_status_codes: response.request_retry_status_codes,
    audit_retention_days: response.audit_retention_days,
    audit_detail_retention_days: response.audit_detail_retention_days,
  };
}

const maxAuditRetentionDays = 36500;

function displayVersion(value?: string) {
  if (!value) return '—';
  return value === 'dev' || value.startsWith('v') ? value : `v${value}`;
}

function validAuditRetentionDays(value: number) {
  return Number.isInteger(value) && value >= 1 && value <= maxAuditRetentionDays;
}

export default function SettingsPage() {
  const [settings, setSettings] = useState<SettingsResponse | null>(null);
  const [form, setForm] = useState<SettingsForm>(emptyForm);
  const [adminPassword, setAdminPassword] = useState('');
  const [signerToken, setSignerToken] = useState('');
  const [loading, setLoading] = useState(true);
  const [saving, setSaving] = useState(false);
  const [checkingUserAgent, setCheckingUserAgent] = useState(false);
  const [replacingUserAgent, setReplacingUserAgent] = useState(false);
  const [userAgentCandidate, setUserAgentCandidate] = useState<UserAgentCheckResponse | null>(null);
  const [versionInfo, setVersionInfo] = useState<ProjectVersionInfo | null>(null);
  const [versionLoading, setVersionLoading] = useState(true);
  const [checkingVersion, setCheckingVersion] = useState(false);
  const [versionRequestError, setVersionRequestError] = useState('');
  const [saved, setSaved] = useState(false);
  const [error, setError] = useState('');
  const [cleanupTarget, setCleanupTarget] = useState<AuditCleanupTarget | null>(null);
  const [cleaningAudit, setCleaningAudit] = useState(false);
  const userAgentConfirmation = useDisclosure();
  const cleanupConfirmation = useDisclosure();
  const { showToast } = useToast();

  const load = useCallback(async () => {
    try {
      const response = await apiFetch<SettingsResponse>('/api/settings');
      setSettings(response);
      setForm(settingsForm(response));
      setError('');
    } catch (requestError) {
      setError(requestError instanceof Error ? requestError.message : '无法加载设置');
    } finally {
      setLoading(false);
    }
  }, []);

  useEffect(() => { void load(); }, [load]);

  useEffect(() => {
    let active = true;
    void apiFetch<ProjectVersionInfo>('/api/version')
      .then((response) => {
        if (!active) return;
        setVersionInfo(response);
        setVersionRequestError('');
      })
      .catch((requestError) => {
        if (active) setVersionRequestError(errorMessage(requestError, '无法读取版本信息'));
      })
      .finally(() => {
        if (active) setVersionLoading(false);
      });
    return () => { active = false; };
  }, []);

  const update = <K extends keyof SettingsForm>(key: K, value: SettingsForm[K]) => {
    setForm((current) => ({ ...current, [key]: value }));
    setSaved(false);
    setError('');
  };

  const save = async (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    if (!form.user_agent.trim() || !form.platform_base_url.trim() || !form.codingplan_api_url.trim() || !form.gateway_url.trim()) {
      setError('必填设置不能为空');
      showToast('error', '保存设置失败', '必填设置不能为空');
      return;
    }
    if (!Number.isInteger(form.request_retry_count) || form.request_retry_count < 0 || form.request_retry_count > 10) {
      setError('请求重试次数必须是 0-10 之间的整数');
      showToast('error', '保存设置失败', '请求重试次数必须是 0-10 之间的整数');
      return;
    }
    if (form.request_retry_count > 0 && !form.request_retry_status_codes.trim()) {
      setError('启用请求重试时必须填写重试状态码');
      showToast('error', '保存设置失败', '启用请求重试时必须填写重试状态码');
      return;
    }
    if (!validAuditRetentionDays(form.audit_retention_days) || !validAuditRetentionDays(form.audit_detail_retention_days)) {
      const message = `审计日志保留天数必须是 1-${maxAuditRetentionDays} 之间的整数`;
      setError(message);
      showToast('error', '保存设置失败', message);
      return;
    }
    setSaving(true);
    setSaved(false);
    try {
      const payload: Record<string, unknown> = { ...form };
      if (adminPassword) payload.admin_password = adminPassword;
      if (signerToken) payload.signer_token = signerToken;
      const response = await apiFetch<SettingsResponse>('/api/settings', jsonRequest('PUT', payload));
      setSettings(response);
      setForm(settingsForm(response));
      setAdminPassword('');
      setSignerToken('');
      setSaved(true);
      setError('');
      showToast('success', '设置保存成功', '新配置已立即生效');
    } catch (requestError) {
      showToast('error', '保存设置失败', errorMessage(requestError, '请检查配置后重试'));
    } finally {
      setSaving(false);
    }
  };

  const checkUserAgent = async () => {
    setCheckingUserAgent(true);
    try {
      const result = await apiFetch<UserAgentCheckResponse>('/api/settings/user-agent/check', { method: 'POST' });
      if (form.user_agent.trim() === result.user_agent) {
        showToast('success', 'User-Agent 已是最新', result.user_agent);
        return;
      }
      setUserAgentCandidate(result);
      userAgentConfirmation.onOpen();
    } catch (requestError) {
      showToast('error', '检查 User-Agent 失败', errorMessage(requestError, '无法读取 AtomCode 版本'));
    } finally {
      setCheckingUserAgent(false);
    }
  };

  const replaceUserAgent = async () => {
    if (!userAgentCandidate) return;
    setReplacingUserAgent(true);
    try {
      const response = await apiFetch<SettingsResponse>('/api/settings', jsonRequest('PUT', { user_agent: userAgentCandidate.user_agent }));
      setSettings(response);
      setForm((current) => ({ ...current, user_agent: response.user_agent }));
      setSaved(false);
      setError('');
      userAgentConfirmation.onClose();
      showToast('success', 'User-Agent 已替换', response.user_agent);
    } catch (requestError) {
      showToast('error', '替换 User-Agent 失败', errorMessage(requestError, '请稍后重试'));
    } finally {
      setReplacingUserAgent(false);
    }
  };

  const requestAuditCleanup = (target: AuditCleanupTarget) => {
    const days = target === 'records' ? form.audit_retention_days : form.audit_detail_retention_days;
    if (!validAuditRetentionDays(days)) {
      showToast('error', '无法清理日志', `保留天数必须是 1-${maxAuditRetentionDays} 之间的整数`);
      return;
    }
    setCleanupTarget(target);
    cleanupConfirmation.onOpen();
  };

  const cleanAuditLogs = async () => {
    if (!cleanupTarget) return;
    const days = cleanupTarget === 'records' ? form.audit_retention_days : form.audit_detail_retention_days;
    setCleaningAudit(true);
    try {
      const endpoint = cleanupTarget === 'records' ? '/api/audit/cleanup/records' : '/api/audit/cleanup/details';
      const result = await apiFetch<AuditCleanupResponse>(endpoint, jsonRequest('POST', { days }));
      cleanupConfirmation.onClose();
      showToast(
        'success',
        cleanupTarget === 'records' ? '审计记录清理完成' : '详细日志清理完成',
        cleanupTarget === 'records'
          ? `已删除 ${result.affected.toLocaleString()} 条 ${days} 天前的审计记录`
          : `已清除 ${result.affected.toLocaleString()} 条 ${days} 天前记录的详细内容`,
      );
      setCleanupTarget(null);
    } catch (requestError) {
      showToast('error', '日志清理失败', errorMessage(requestError, '请稍后重试'));
    } finally {
      setCleaningAudit(false);
    }
  };

  const checkApplicationVersion = async () => {
    setCheckingVersion(true);
    setVersionRequestError('');
    try {
      const response = await apiFetch<ProjectVersionInfo>('/api/version?refresh=1');
      setVersionInfo(response);
      if (response.check_error) {
        showToast('error', '检查更新失败', response.check_error);
      } else if (response.update_available) {
        showToast('success', '发现新版本', `最新稳定版本为 ${displayVersion(response.latest_version)}`);
      } else if (response.current_version === 'dev') {
        showToast('success', '检查完成', `当前为开发构建，最新稳定版本为 ${displayVersion(response.latest_version)}`);
      } else {
        showToast('success', '已是最新版本', displayVersion(response.current_version));
      }
    } catch (requestError) {
      const message = errorMessage(requestError, '无法读取版本信息');
      setVersionRequestError(message);
      showToast('error', '检查更新失败', message);
    } finally {
      setCheckingVersion(false);
    }
  };

  const isDirty = Boolean(settings) && (
    form.user_agent !== settings?.user_agent || form.platform_base_url !== settings?.platform_base_url ||
    form.codingplan_api_url !== settings?.codingplan_api_url || form.gateway_url !== settings?.gateway_url ||
    form.signer_url !== settings?.signer_url || form.audit_debug_enabled !== settings?.audit_debug_enabled ||
    form.system_prompt_enabled !== settings?.system_prompt_enabled || form.system_prompt !== settings?.system_prompt ||
    form.request_timeout_seconds !== settings?.request_timeout_seconds || form.request_retry_count !== settings?.request_retry_count ||
    form.request_retry_status_codes !== settings?.request_retry_status_codes || form.audit_retention_days !== settings?.audit_retention_days ||
    form.audit_detail_retention_days !== settings?.audit_detail_retention_days ||
    Boolean(adminPassword) || Boolean(signerToken)
  );
  const repositoryURL = versionInfo?.repository_url || '';
  const developmentBuild = versionInfo?.current_version === 'dev';

  return (
    <PageShell title="系统设置" description="服务配置、项目信息与版本更新">
      {settings?.default_password ? <div className="flex items-start gap-3 rounded-md border border-amber-200 bg-amber-50 px-4 py-3 text-sm text-amber-800" role="alert"><ShieldAlert className="mt-0.5 shrink-0" size={17} /><span>当前仍使用默认管理密码，请在下方设置新密码。</span></div> : null}
      {error ? <div className="flex items-start gap-2 rounded-md border border-red-200 bg-red-50 px-4 py-3 text-sm text-red-700" role="alert"><AlertCircle className="mt-0.5 shrink-0" size={16} />{error}</div> : null}

      <section className="overflow-hidden rounded-lg border border-zinc-200 bg-white">
        <div className="flex flex-col gap-3 border-b border-zinc-100 px-5 py-4 sm:flex-row sm:items-center sm:justify-between sm:px-6">
          <div className="flex items-center gap-3"><span className="flex h-9 w-9 shrink-0 items-center justify-center rounded-lg bg-zinc-900 text-white"><Github size={18} /></span><div><h2 className="text-sm font-semibold text-zinc-900">项目信息</h2><p className="mt-0.5 text-xs text-zinc-500">程序版本、源代码仓库与更新状态</p></div></div>
          <Button isDisabled={versionLoading} isLoading={checkingVersion} radius="sm" size="sm" startContent={checkingVersion ? null : <RefreshCw size={15} />} type="button" variant="bordered" onPress={() => void checkApplicationVersion()}>检查更新</Button>
        </div>
        {versionLoading ? (
          <div className="grid gap-4 px-5 py-6 sm:grid-cols-3 sm:px-6">{Array.from({ length: 3 }).map((_, index) => <Skeleton key={index} className="h-12 w-full rounded-md" />)}</div>
        ) : (
          <div>
            <dl className="grid gap-5 px-5 py-5 sm:grid-cols-3 sm:px-6">
              <div><dt className="text-xs text-zinc-500">当前版本</dt><dd className="mt-1 font-mono text-sm font-semibold text-zinc-900">{displayVersion(versionInfo?.current_version)}</dd></div>
              <div><dt className="text-xs text-zinc-500">最新稳定版本</dt><dd className="mt-1 font-mono text-sm font-semibold text-zinc-900">{displayVersion(versionInfo?.latest_version)}</dd></div>
              <div><dt className="text-xs text-zinc-500">最近检查</dt><dd className="mt-1 text-sm font-medium text-zinc-800" title={versionInfo?.checked_at}>{versionInfo?.checked_at ? formatDateTime(versionInfo.checked_at) : '尚未检查'}</dd></div>
            </dl>
            <dl className="border-t border-zinc-100 px-5 py-4 sm:px-6">
              {repositoryURL ? <div className="grid gap-1 sm:grid-cols-[140px_minmax(0,1fr)] sm:items-center"><dt className="text-xs text-zinc-500">GitHub 仓库</dt><dd className="min-w-0"><a className="inline-flex max-w-full items-center gap-1.5 break-all font-mono text-xs font-medium text-blue-600 hover:text-blue-800" href={repositoryURL} rel="noreferrer" target="_blank"><span>{repositoryURL}</span><ExternalLink className="shrink-0" size={14} /></a></dd></div> : null}
            </dl>
            <div className="border-t border-zinc-100 px-5 py-4 sm:px-6">
              {versionRequestError || versionInfo?.check_error ? <div className="flex items-start gap-2 rounded-md border border-red-200 bg-red-50 px-3 py-2.5 text-sm text-red-700" role="alert"><AlertCircle className="mt-0.5 shrink-0" size={16} /><span>更新检查失败：{versionRequestError || versionInfo?.check_error}</span></div>
                : versionInfo?.update_available ? <div className="flex flex-col gap-3 rounded-md border border-amber-200 bg-amber-50 px-3 py-2.5 text-sm text-amber-800 sm:flex-row sm:items-center sm:justify-between"><span className="flex items-center gap-2"><ArrowUpCircle className="shrink-0" size={16} />发现新版本 {displayVersion(versionInfo.latest_version)}</span>{versionInfo.release_url ? <Button as="a" className="text-amber-800" endContent={<ExternalLink size={14} />} href={versionInfo.release_url} radius="sm" rel="noreferrer" size="sm" target="_blank" variant="light">查看版本</Button> : null}</div>
                  : developmentBuild ? <div className="flex items-start gap-2 rounded-md border border-blue-200 bg-blue-50 px-3 py-2.5 text-sm text-blue-700"><AlertCircle className="mt-0.5 shrink-0" size={16} /><span>当前运行开发构建，无法与正式版本自动比较；GitHub 最新稳定版本为 {displayVersion(versionInfo?.latest_version)}。</span></div>
                    : versionInfo?.latest_version ? <div className="flex items-center gap-2 rounded-md border border-emerald-200 bg-emerald-50 px-3 py-2.5 text-sm text-emerald-700"><Check className="shrink-0" size={16} />当前已是最新稳定版本</div>
                      : <div className="flex items-center gap-2 rounded-md border border-zinc-200 bg-zinc-50 px-3 py-2.5 text-sm text-zinc-600"><AlertCircle className="shrink-0" size={16} />尚未获得最新版本信息</div>}
            </div>
            {versionInfo?.release_notes ? <div className="border-t border-zinc-100 px-5 py-5 sm:px-6"><div className="mb-2 flex items-center justify-between gap-3"><h3 className="text-sm font-semibold text-zinc-900">最新版本更新日志</h3>{versionInfo.published_at ? <span className="text-xs text-zinc-400">发布于 {formatDateTime(versionInfo.published_at)}</span> : null}</div><pre className="max-h-64 overflow-auto whitespace-pre-wrap break-words rounded-md bg-zinc-50 p-4 font-mono text-xs leading-5 text-zinc-600">{versionInfo.release_notes}</pre></div> : null}
          </div>
        )}
      </section>

      <form className="space-y-5" onSubmit={save}>
        <section className="overflow-hidden rounded-lg border border-zinc-200 bg-white">
          <div className="flex items-center justify-between border-b border-zinc-100 px-5 py-4 sm:px-6"><div className="flex items-center gap-3"><span className="flex h-9 w-9 items-center justify-center rounded-lg bg-blue-50 text-blue-600"><Network size={18} /></span><div><h2 className="text-sm font-semibold text-zinc-900">服务端点</h2><p className="mt-0.5 text-xs text-zinc-500">OAuth、权益和模型网关</p></div></div><Chip className="border-zinc-200 bg-white text-zinc-600" radius="sm" size="sm" startContent={<StatusDot tone={error ? 'danger' : 'success'} />} variant="bordered">{error ? '配置异常' : '已加载'}</Chip></div>
          {loading ? <div className="grid gap-5 px-5 py-6 sm:grid-cols-2 sm:px-6">{Array.from({ length: 4 }).map((_, index) => <Skeleton key={index} className="h-14 w-full rounded-md" />)}</div> : <div className="grid gap-5 px-5 py-6 sm:grid-cols-2 sm:px-6"><Input isRequired label="Platform OAuth" labelPlacement="outside" radius="sm" value={form.platform_base_url} onValueChange={(value) => update('platform_base_url', value)} /><Input isRequired label="Coding Plan API" labelPlacement="outside" radius="sm" value={form.codingplan_api_url} onValueChange={(value) => update('codingplan_api_url', value)} /><Input isRequired label="LLM Gateway" labelPlacement="outside" radius="sm" value={form.gateway_url} onValueChange={(value) => update('gateway_url', value)} /><Input label="请求签名服务" labelPlacement="outside" placeholder="https://signer.example.com/v1/sign" radius="sm" value={form.signer_url} onValueChange={(value) => update('signer_url', value)} /></div>}
        </section>

        <section className="overflow-hidden rounded-lg border border-zinc-200 bg-white">
          <div className="flex items-center justify-between gap-3 border-b border-zinc-100 px-5 py-4 sm:px-6"><div className="flex items-center gap-3"><span className="flex h-9 w-9 items-center justify-center rounded-lg bg-emerald-50 text-emerald-700"><Settings2 size={18} /></span><div><h2 className="text-sm font-semibold text-zinc-900">系统提示词</h2><p className="mt-0.5 text-xs text-zinc-500">向模型请求自动添加一段系统指令</p></div></div><Switch aria-label="切换系统提示词" color="primary" isSelected={form.system_prompt_enabled} onValueChange={(selected) => update('system_prompt_enabled', selected)}><span className="text-sm text-zinc-600">{form.system_prompt_enabled ? '已开启' : '已关闭'}</span></Switch></div>
          <div className="space-y-4 px-5 py-6 sm:px-6"><Textarea description="{model} 会替换为当前请求的模型名称" label="提示词模板" labelPlacement="outside" minRows={6} radius="sm" value={form.system_prompt} classNames={{ input: 'font-mono text-sm' }} onValueChange={(value) => update('system_prompt', value)} /><div className="flex justify-end"><Button radius="sm" startContent={<RotateCcw size={15} />} type="button" variant="flat" onPress={() => update('system_prompt', settings?.default_system_prompt || '')}>重置为默认</Button></div></div>
        </section>

        <section className="overflow-hidden rounded-lg border border-zinc-200 bg-white">
          <div className="flex items-center gap-3 border-b border-zinc-100 px-5 py-4 sm:px-6"><span className="flex h-9 w-9 items-center justify-center rounded-lg bg-zinc-100 text-zinc-600"><Settings2 size={18} /></span><div><h2 className="text-sm font-semibold text-zinc-900">请求参数</h2><p className="mt-0.5 text-xs text-zinc-500">上游客户端标识与超时</p></div></div>
          <div className="grid gap-5 px-5 py-6 sm:grid-cols-[minmax(0,1fr)_220px] sm:px-6">
            <Input isRequired label="User-Agent" labelPlacement="outside" radius="sm" value={form.user_agent} classNames={{ input: 'font-mono text-sm' }} endContent={<Tooltip content="检查 AtomCode 最新版本"><Button isIconOnly aria-label="检查 AtomCode User-Agent" isDisabled={loading} isLoading={checkingUserAgent} radius="sm" size="sm" type="button" variant="light" onPress={() => void checkUserAgent()}>{checkingUserAgent ? null : <RefreshCw size={16} />}</Button></Tooltip>} onValueChange={(value) => update('user_agent', value)} />
            <Input isRequired label="请求超时（秒）" labelPlacement="outside" max={600} min={5} radius="sm" type="number" value={String(form.request_timeout_seconds)} onValueChange={(value) => update('request_timeout_seconds', Number(value) || 0)} />
            <Input description="逗号分隔，可输入范围或单个状态码；保存后自动归并" isRequired={form.request_retry_count > 0} label="重试状态码" labelPlacement="outside" placeholder="400-500,503,429" radius="sm" value={form.request_retry_status_codes} classNames={{ input: 'font-mono text-sm' }} onValueChange={(value) => update('request_retry_status_codes', value)} />
            <Input description="0 表示请求失败后不重试" isRequired label="请求重试次数" labelPlacement="outside" max={10} min={0} radius="sm" step={1} type="number" value={String(form.request_retry_count)} onValueChange={(value) => update('request_retry_count', Number(value) || 0)} />
          </div>
        </section>

        <section className="overflow-hidden rounded-lg border border-zinc-200 bg-white">
          <div className="flex items-center gap-3 border-b border-zinc-100 px-5 py-4 sm:px-6"><span className="flex h-9 w-9 items-center justify-center rounded-lg bg-violet-50 text-violet-700"><Bug size={18} /></span><div><h2 className="text-sm font-semibold text-zinc-900">请求审计</h2><p className="mt-0.5 text-xs text-zinc-500">详细内容记录与日志清理策略</p></div></div>
          <div className="flex flex-col gap-4 px-5 py-5 sm:flex-row sm:items-center sm:justify-between sm:px-6">
            <div><p className="text-sm font-medium text-zinc-800">审计调试模式</p><p className="mt-1 text-xs leading-5 text-amber-700">完整内容可能包含提示词、代码和其他敏感数据</p></div>
            <Switch aria-label="切换审计调试模式" color="primary" isSelected={form.audit_debug_enabled} onValueChange={(selected) => update('audit_debug_enabled', selected)}><span className="text-sm text-zinc-600">{form.audit_debug_enabled ? '已开启' : '已关闭'}</span></Switch>
          </div>
          <div className="grid border-t border-zinc-100 md:grid-cols-2 md:divide-x md:divide-zinc-100">
            <div className="space-y-4 px-5 py-5 sm:px-6">
              <div><p className="text-sm font-medium text-zinc-800">清理请求审计记录</p><p className="mt-1 text-xs leading-5 text-zinc-500">永久删除超过保留天数的完整审计记录，不影响账号和密钥累计统计。</p></div>
              <Input isRequired description="默认 30 天；保存设置后作为下次默认值" label="审计记录保留天数" classNames={{ base: "!mt-8" }} labelPlacement="outside" max={maxAuditRetentionDays} min={1} radius="sm" step={1} type="number" value={String(form.audit_retention_days)} onValueChange={(value) => update('audit_retention_days', Number(value) || 0)} />
              <Button color="danger" isDisabled={loading} radius="sm" startContent={<Trash2 size={15} />} type="button" variant="flat" onPress={() => requestAuditCleanup('records')}>清理过期记录</Button>
            </div>
            <div className="space-y-4 border-t border-zinc-100 px-5 py-5 sm:px-6 md:border-t-0">
              <div><p className="text-sm font-medium text-zinc-800">清理记录的详细日志</p><p className="mt-1 text-xs leading-5 text-zinc-500">保留审计摘要，仅清除过期记录的正文、Header、错误及重试详情。</p></div>
              <Input isRequired description="默认 30 天；保存设置后作为下次默认值" label="详细日志保留天数" classNames={{ base: "!mt-8" }} labelPlacement="outside" max={maxAuditRetentionDays} min={1} radius="sm" step={1} type="number" value={String(form.audit_detail_retention_days)} onValueChange={(value) => update('audit_detail_retention_days', Number(value) || 0)} />
              <Button color="danger" isDisabled={loading} radius="sm" startContent={<Trash2 size={15} />} type="button" variant="flat" onPress={() => requestAuditCleanup('details')}>清理过期详情</Button>
            </div>
          </div>
        </section>

        <section className="overflow-hidden rounded-lg border border-zinc-200 bg-white">
          <div className="flex items-center gap-3 border-b border-zinc-100 px-5 py-4 sm:px-6"><span className="flex h-9 w-9 items-center justify-center rounded-lg bg-amber-50 text-amber-700"><KeyRound size={18} /></span><div><h2 className="text-sm font-semibold text-zinc-900">敏感凭据</h2><p className="mt-0.5 text-xs text-zinc-500">留空表示保持当前值</p></div></div>
          <div className="grid gap-5 px-5 py-6 sm:grid-cols-2 sm:px-6"><Input label="新管理密码" labelPlacement="outside" placeholder="至少使用高强度随机密码" radius="sm" type="password" value={adminPassword} onValueChange={(value) => { setAdminPassword(value); setSaved(false); }} /><Input label="签名服务 Token" labelPlacement="outside" placeholder={settings?.signer_configured ? '已配置' : '未配置'} radius="sm" type="password" value={signerToken} onValueChange={(value) => { setSignerToken(value); setSaved(false); }} /></div>
        </section>

        <div className="flex flex-col-reverse gap-3 sm:flex-row sm:items-center sm:justify-between"><span className="text-xs text-zinc-400">配置文件：{settings?.data_path ? 'config.json' : '—'}</span><div className="flex items-center justify-end gap-3">{saved ? <span className="flex items-center gap-1.5 text-sm text-emerald-700" role="status"><Check size={16} />已保存</span> : null}<Button color="primary" isDisabled={!isDirty} isLoading={saving} radius="sm" startContent={saving ? null : <Save size={16} />} type="submit">保存设置</Button></div></div>
      </form>

      <Modal isDismissable={!cleaningAudit} isKeyboardDismissDisabled={cleaningAudit} isOpen={cleanupConfirmation.isOpen} radius="sm" size="md" onOpenChange={cleanupConfirmation.onOpenChange}>
        <ModalContent>{(onClose) => <><ModalHeader>{cleanupTarget === 'records' ? '确认清理审计记录' : '确认清理详细日志'}</ModalHeader><ModalBody><div className="rounded-md border border-red-200 bg-red-50 px-4 py-3 text-sm leading-6 text-red-700">{cleanupTarget === 'records' ? <>将永久删除 <strong>{form.audit_retention_days} 天前</strong>的全部请求审计记录，此操作不可恢复。</> : <>将清除 <strong>{form.audit_detail_retention_days} 天前</strong>审计记录中的正文、Header、错误和重试详情，摘要记录仍会保留。此操作不可恢复。</>}</div><p className="text-xs leading-5 text-zinc-500">清理使用当前输入的天数；如需下次继续使用该值，请点击页面底部的“保存设置”。</p></ModalBody><ModalFooter><Button isDisabled={cleaningAudit} radius="sm" variant="light" onPress={onClose}>取消</Button><Button color="danger" isLoading={cleaningAudit} radius="sm" startContent={cleaningAudit ? null : <Trash2 size={15} />} onPress={() => void cleanAuditLogs()}>确认清理</Button></ModalFooter></>}</ModalContent>
      </Modal>

      <Modal isOpen={userAgentConfirmation.isOpen} radius="sm" size="sm" onOpenChange={userAgentConfirmation.onOpenChange}>
        <ModalContent>{(onClose) => <><ModalHeader>更新 User-Agent</ModalHeader><ModalBody><p className="text-sm leading-6 text-zinc-600">AtomCode 当前版本为 <code className="font-mono font-semibold text-zinc-900">{userAgentCandidate?.version}</code>，是否将 User-Agent 从 <code className="break-all font-mono text-zinc-700">{form.user_agent}</code> 替换为 <code className="break-all font-mono font-semibold text-zinc-900">{userAgentCandidate?.user_agent}</code>？</p></ModalBody><ModalFooter><Button radius="sm" variant="light" onPress={onClose}>保留当前值</Button><Button color="primary" isLoading={replacingUserAgent} radius="sm" onPress={() => void replaceUserAgent()}>替换</Button></ModalFooter></>}</ModalContent>
      </Modal>
    </PageShell>
  );
}
