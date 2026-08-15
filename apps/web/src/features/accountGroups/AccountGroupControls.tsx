import type { CSSProperties } from 'react';
import { useTranslation } from 'react-i18next';
import { IconCheck } from '@/components/ui/icons';
import type { AccountGroup } from '@/services/api';
import {
  normalizeAccountGroupColor,
  normalizeAccountGroupIds,
  resolveAccountGroups,
} from './accountGroupModel';
import styles from './AccountGroupControls.module.scss';

type GroupStyle = CSSProperties & { '--group-color': string };

export function AccountGroupBadges({
  ids,
  groups,
  maxVisible = 3,
  showEmpty = false,
}: {
  ids: number[];
  groups: AccountGroup[];
  maxVisible?: number;
  showEmpty?: boolean;
}) {
  const { t } = useTranslation();
  const normalizedIds = normalizeAccountGroupIds(ids);
  const resolved = resolveAccountGroups(normalizedIds, groups);
  const visible = resolved.groups.slice(0, Math.max(0, maxVisible));
  const hiddenCount = Math.max(0, resolved.groups.length - visible.length);

  if (normalizedIds.length === 0) {
    return showEmpty ? (
      <span className={styles.emptyBadge}>{t('account_groups.ungrouped')}</span>
    ) : null;
  }

  return (
    <span className={styles.badges}>
      {visible.map((group) => {
        const color = normalizeAccountGroupColor(group.color);
        return (
          <span
            key={group.id}
            className={styles.badge}
            style={{ '--group-color': color } as GroupStyle}
            title={group.description || group.name}
          >
            <span className={styles.dot} />
            <span className={styles.badgeLabel}>{group.name}</span>
          </span>
        );
      })}
      {hiddenCount > 0 ? <span className={styles.missingBadge}>+{hiddenCount}</span> : null}
      {resolved.missingIds.length > 0 ? (
        <span
          className={styles.missingBadge}
          title={resolved.missingIds.map((id) => `#${id}`).join(', ')}
        >
          {t('account_groups.missing_count', { count: resolved.missingIds.length })}
        </span>
      ) : null}
    </span>
  );
}

export function AccountGroupPicker({
  groups,
  value,
  onChange,
  disabled = false,
}: {
  groups: AccountGroup[];
  value: number[];
  onChange: (value: number[]) => void;
  disabled?: boolean;
}) {
  const { t } = useTranslation();
  const normalizedValue = normalizeAccountGroupIds(value);
  const selected = new Set(normalizedValue);

  const toggle = (id: number) => {
    if (disabled) return;
    const next = new Set(selected);
    if (next.has(id)) next.delete(id);
    else next.add(id);
    onChange(Array.from(next).sort((left, right) => left - right));
  };

  return (
    <div className={styles.picker}>
      {groups.length === 0 ? (
        <div className={styles.pickerEmpty}>{t('account_groups.no_groups')}</div>
      ) : (
        <div className={styles.pickerGrid}>
          {groups.map((group) => {
            const checked = selected.has(group.id);
            const color = normalizeAccountGroupColor(group.color);
            return (
              <button
                key={group.id}
                type="button"
                className={`${styles.pickerOption} ${checked ? styles.pickerOptionSelected : ''}`}
                style={{ '--group-color': color } as GroupStyle}
                onClick={() => toggle(group.id)}
                disabled={disabled}
                aria-pressed={checked}
              >
                <span className={styles.pickerCheck}>
                  {checked ? <IconCheck size={13} /> : null}
                </span>
                <span className={styles.pickerCopy}>
                  <strong>{group.name}</strong>
                  <small>{group.description || t('account_groups.no_description')}</small>
                </span>
                <span className={styles.pickerCount}>{group.member_count}</span>
              </button>
            );
          })}
        </div>
      )}
      <div className={styles.pickerFooter}>
        <span>{t('account_groups.selected_count', { count: normalizedValue.length })}</span>
        <button
          type="button"
          className={styles.clearButton}
          onClick={() => onChange([])}
          disabled={disabled || normalizedValue.length === 0}
        >
          {t('account_groups.clear_selection')}
        </button>
      </div>
    </div>
  );
}
