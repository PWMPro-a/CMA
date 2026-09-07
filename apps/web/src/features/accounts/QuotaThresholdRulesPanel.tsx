import { forwardRef, useCallback, useEffect, useImperativeHandle, useMemo, useState } from 'react';
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
  onRulesChange?: (rules: QuotaThresholdRule[]) => void;
};

export type QuotaThresholdRulesPanelHandle = {
  open: () => void;
};

export const QuotaThresholdRulesPanel = forwardRef<QuotaThresholdRulesPanelHandle, Props>(
  function QuotaThresholdRulesPanel(
    { selectedRows, managerServiceBase, managementKey, disabled, onRulesChange },
    ref
  ) {
    const { t } = useTranslation();
    const showNotification = useNotificationStore((state) => state.showNotification);
    const [saving, setSaving] = useState(false);
    const [open, setOpen] = useState(false);
    const [threshold, setThreshold] = useState('20');
    const [enabled, setEnabled] = useState(true);

    useImperativeHandle(ref, () => ({ open: () => setOpen(true) }), []);

    const codexRows = useMemo(
      () => selectedRows.filter((row) => !row.runtimeOnly && row.provider === CODEX_CONFIG.type),
      [selectedRows]
    );
    const load = useCallback(async () => {
      if (!managerServiceBase || !managementKey) {
        onRulesChange?.([]);
        return;
      }
      try {
        onRulesChange?.(await quotaThresholdRulesApi.list(managerServiceBase, managementKey));
      } catch (error) {
        showNotification(
          error instanceof Error ? error.message : t('common.unknown_error'),
          'error'
        );
      }
    }, [managerServiceBase, managementKey, onRulesChange, showNotification, t]);
    useEffect(() => {
      void load();
    }, [load]);

    const save = useCallback(async () => {
      const value = Number(threshold);
      if (!Number.isFinite(value) || value < 0 || value > 100) {
        showNotification(t('accounts.quota_threshold_invalid'), 'error');
        return;
      }
      if (!managerServiceBase || !managementKey || codexRows.length === 0) {
        showNotification(t('accounts.quota_threshold_select_codex'), 'info');
        return;
      }
      setSaving(true);
      try {
        const next = codexRows.map((row) => {
          const target = getAuthFilePatchTarget(row.raw);
          return {
            fileName: target.name,
            authIndex: target.authIndex == null ? '' : String(target.authIndex),
            provider: target.provider || row.provider,
            accountSnapshot: target.accountSnapshot || '',
            accountId: target.accountId || '',
            thresholdPercent: value,
            enabled,
          };
        });
        onRulesChange?.(await quotaThresholdRulesApi.save(managerServiceBase, managementKey, next));
        setOpen(false);
        showNotification(
          t('accounts.quota_threshold_saved', { count: codexRows.length }),
          'success'
        );
      } catch (error) {
        showNotification(
          error instanceof Error ? error.message : t('common.unknown_error'),
          'error'
        );
      } finally {
        setSaving(false);
      }
    }, [
      codexRows,
      enabled,
      managementKey,
      managerServiceBase,
      onRulesChange,
      showNotification,
      t,
      threshold,
    ]);

    if (!managerServiceBase || !managementKey) return null;
    return (
      <>
        <Modal
          open={open}
          onClose={() => {
            if (!saving) setOpen(false);
          }}
          closeDisabled={saving || disabled}
          title={t('accounts.quota_threshold_modal_title', { count: codexRows.length })}
          width={460}
          footer={
            <div className={styles.footer}>
              <Button
                size="sm"
                variant="secondary"
                onClick={() => setOpen(false)}
                disabled={saving || disabled}
              >
                {t('common.cancel')}
              </Button>
              <Button
                size="sm"
                onClick={() => void save()}
                loading={saving}
                disabled={saving || disabled}
              >
                {t('common.confirm')}
              </Button>
            </div>
          }
        >
          <div className={styles.form}>
            <Input
              label={t('accounts.quota_threshold_input_label')}
              hint={t('accounts.quota_threshold_input_hint')}
              value={threshold}
              onChange={(event) => setThreshold(event.target.value)}
              inputMode="decimal"
              autoFocus
              disabled={saving || disabled}
            />
            <ToggleSwitch
              checked={enabled}
              onChange={setEnabled}
              ariaLabel={t('accounts.quota_threshold_enabled')}
              label={t('accounts.quota_threshold_enabled')}
              disabled={saving || disabled}
            />
            <p className={styles.explain}>{t('accounts.quota_threshold_explain')}</p>
          </div>
        </Modal>
      </>
    );
  }
);

QuotaThresholdRulesPanel.displayName = 'QuotaThresholdRulesPanel';
