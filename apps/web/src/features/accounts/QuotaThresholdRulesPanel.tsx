import { useCallback, useEffect, useMemo, useState } from 'react';
import { useTranslation } from 'react-i18next';
import { Button } from '@/components/ui/Button';
import { Input } from '@/components/ui/Input';
import { Modal } from '@/components/ui/Modal';
import { ToggleSwitch } from '@/components/ui/ToggleSwitch';
import { useNotificationStore } from '@/stores';
import { getAuthFilePatchTarget } from '@/features/authFiles/model/authFilesPageModel';
import { CODEX_CONFIG } from '@/components/quota';
import { quotaThresholdRulesApi, type QuotaThresholdRule } from '@/services/api';
import type { AccountRow } from '@/features/accounts/model/accountRows';
import styles from './QuotaThresholdRulesPanel.module.scss';

type Props = {
  selectedRows: AccountRow[];
  managerServiceBase?: string;
  managementKey?: string;
  disabled?: boolean;
};

const formatPercent = (value: number | undefined) =>
  typeof value === 'number' && Number.isFinite(value) ? `${value.toFixed(1)}%` : '—';

export function QuotaThresholdRulesPanel({ selectedRows, managerServiceBase, managementKey, disabled }: Props) {
  const { t } = useTranslation();
  const showNotification = useNotificationStore((state) => state.showNotification);
  const [rules, setRules] = useState<QuotaThresholdRule[]>([]);
  const [loading, setLoading] = useState(false);
  const [saving, setSaving] = useState(false);
  const [open, setOpen] = useState(false);
  const [threshold, setThreshold] = useState('20');
  const [enabled, setEnabled] = useState(true);

  const codexRows = useMemo(
    () => selectedRows.filter((row) => !row.runtimeOnly && row.provider === CODEX_CONFIG.type),
    [selectedRows]
  );
  const load = useCallback(async () => {
    if (!managerServiceBase || !managementKey) { setRules([]); return; }
    setLoading(true);
    try { setRules(await quotaThresholdRulesApi.list(managerServiceBase, managementKey)); }
    catch (error) { showNotification(error instanceof Error ? error.message : t('common.unknown_error'), 'error'); }
    finally { setLoading(false); }
  }, [managerServiceBase, managementKey, showNotification, t]);
  useEffect(() => { void load(); }, [load]);

  const save = useCallback(async () => {
    const value = Number(threshold);
    if (!Number.isFinite(value) || value < 0 || value > 100) { showNotification(t('accounts.quota_threshold_invalid'), 'error'); return; }
    if (!managerServiceBase || !managementKey || codexRows.length === 0) { showNotification(t('accounts.quota_threshold_select_codex'), 'info'); return; }
    setSaving(true);
    try {
      const next = codexRows.map((row) => {
        const target = getAuthFilePatchTarget(row.raw);
        return { fileName: target.name, authIndex: target.authIndex == null ? '' : String(target.authIndex), provider: target.provider || row.provider, accountSnapshot: target.accountSnapshot || '', accountId: target.accountId || '', thresholdPercent: value, enabled };
      });
      setRules(await quotaThresholdRulesApi.save(managerServiceBase, managementKey, next));
      setOpen(false);
      showNotification(t('accounts.quota_threshold_saved', { count: codexRows.length }), 'success');
    } catch (error) { showNotification(error instanceof Error ? error.message : t('common.unknown_error'), 'error'); }
    finally { setSaving(false); }
  }, [codexRows, enabled, managementKey, managerServiceBase, showNotification, t, threshold]);

  const remove = useCallback(async (id: number) => {
    if (!managerServiceBase || !managementKey) return;
    try { await quotaThresholdRulesApi.remove(managerServiceBase, managementKey, id); setRules((current) => current.filter((rule) => rule.id !== id)); showNotification(t('accounts.quota_threshold_deleted'), 'success'); }
    catch (error) { showNotification(error instanceof Error ? error.message : t('common.unknown_error'), 'error'); }
  }, [managementKey, managerServiceBase, showNotification, t]);

  if (!managerServiceBase || !managementKey) return null;
  return (
    <section className={styles.panel} aria-label={t('accounts.quota_threshold_title')}>
      <div className={styles.header}>
        <div>
          <h3>{t('accounts.quota_threshold_title')}</h3>
          <p>{t('accounts.quota_threshold_description')}</p>
        </div>
        <Button size="sm" variant="secondary" onClick={() => setOpen(true)} disabled={disabled || codexRows.length === 0}>
          {t('accounts.quota_threshold_configure', { count: codexRows.length })}
        </Button>
      </div>
      {loading ? <p className={styles.muted}>{t('common.loading')}</p> : rules.length === 0 ? <p className={styles.muted}>{t('accounts.quota_threshold_empty')}</p> : (
        <div className={styles.list}>
          {rules.map((rule) => <div className={styles.row} key={rule.id}>
            <div className={styles.identity}><strong>{rule.accountSnapshot || rule.accountId || rule.fileName}</strong><small>{rule.fileName}{rule.authIndex ? ` · ${rule.authIndex}` : ''}</small></div>
            <span>{t('accounts.quota_threshold_rule_label', { percent: rule.thresholdPercent })}</span>
            <span>{t('accounts.quota_threshold_current_label', { percent: formatPercent(rule.lastObservedRemainingPercent) })}</span>
            <span className={rule.enabled ? styles.enabled : styles.disabled}>{rule.enabled ? t('accounts.quota_threshold_enabled') : t('accounts.quota_threshold_disabled')}{rule.lastDisabled ? ` · ${t('accounts.quota_threshold_account_disabled')}` : ''}</span>
            <Button size="xs" variant="ghost" onClick={() => void remove(rule.id)} disabled={saving}>{t('common.delete')}</Button>
          </div>)}
        </div>
      )}
      <Modal open={open} onClose={() => { if (!saving) setOpen(false); }} closeDisabled={saving} title={t('accounts.quota_threshold_modal_title', { count: codexRows.length })} width={460} footer={<div className={styles.footer}><Button size="sm" variant="secondary" onClick={() => setOpen(false)} disabled={saving}>{t('common.cancel')}</Button><Button size="sm" onClick={() => void save()} loading={saving} disabled={saving}>{t('common.confirm')}</Button></div>}>
        <div className={styles.form}>
          <Input label={t('accounts.quota_threshold_input_label')} hint={t('accounts.quota_threshold_input_hint')} value={threshold} onChange={(event) => setThreshold(event.target.value)} inputMode="decimal" autoFocus disabled={saving} />
          <ToggleSwitch checked={enabled} onChange={setEnabled} ariaLabel={t('accounts.quota_threshold_enabled')} label={t('accounts.quota_threshold_enabled')} disabled={saving} />
          <p className={styles.explain}>{t('accounts.quota_threshold_explain')}</p>
        </div>
      </Modal>
    </section>
  );
}
