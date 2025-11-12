# Dark Mode Testing Guide

This document provides comprehensive testing procedures for the dark/light mode toggle implementation.

## Pre-Testing Setup

1. Install all dependencies: `npm install`
2. Start dev server: `npm run dev`
3. Open browser to `http://localhost:3000`
4. Open DevTools (F12) to monitor console for errors

## Test Categories

### 1. Visual Testing

#### Light Mode
- [ ] Background is white (`#FFFFFF`)
- [ ] Text is dark gray/black (`#1A1A1A`)
- [ ] All elements are clearly visible
- [ ] No contrast issues
- [ ] Images display properly
- [ ] Code blocks have light background

#### Dark Mode
- [ ] Background is very dark (`#0F0F0F`)
- [ ] Text is white/light gray (`#FFFFFF`)
- [ ] All elements are clearly visible
- [ ] No contrast issues
- [ ] Images are slightly dimmed (opacity 90%)
- [ ] Code blocks have dark background

#### Transitions
- [ ] Colors transition smoothly when toggling (no harsh jumps)
- [ ] Transition duration is ~200-300ms
- [ ] No flickering or flashing

### 2. Functional Testing

#### Toggle Button
- [ ] Toggle button appears in navbar
- [ ] Toggle button is clickable
- [ ] Clicking toggles between light and dark modes
- [ ] Toggle icon changes (sun for light, moon for dark)
- [ ] Toggle has hover effect
- [ ] Toggle has proper cursor (pointer)

#### Dropdown Menu (if using ThemeToggle)
- [ ] Dropdown opens on click
- [ ] Dropdown shows 3 options: Light, Dark, System
- [ ] Clicking each option applies correct theme
- [ ] Current theme option is highlighted
- [ ] Dropdown closes after selection
- [ ] Clicking outside dropdown closes it

#### Theme Persistence
- [ ] Select Light mode, reload page → stays Light
- [ ] Select Dark mode, reload page → stays Dark
- [ ] Select System mode, reload page → uses system preference
- [ ] Switch between tabs → consistent theme
- [ ] Close and reopen browser → theme persists (localStorage)

### 3. Responsive Testing

#### Desktop (1920px+)
- [ ] Toggle button visible and properly positioned
- [ ] All text readable
- [ ] No layout shifts
- [ ] All elements fit without horizontal scroll

#### Tablet (768px - 1024px)
- [ ] Toggle button visible
- [ ] Navigation adapts properly
- [ ] Touch targets are appropriately sized (min 44x44px)
- [ ] No content overlap

#### Mobile (320px - 480px)
- [ ] Toggle button visible and accessible
- [ ] Hamburger menu works
- [ ] Toggle in menu (if using hamburger menu)
- [ ] No horizontal scroll
- [ ] Touch targets properly sized
- [ ] Text remains readable

### 4. Accessibility Testing

#### Keyboard Navigation
- [ ] Toggle button is reachable with Tab key
- [ ] Toggle button is activatable with Enter/Space
- [ ] No focus visible when using mouse
- [ ] Focus visible when using keyboard
- [ ] Tab order makes sense
- [ ] No keyboard traps

#### Screen Reader
- [ ] Toggle button has proper ARIA label
- [ ] Label describes action (e.g., "Toggle theme menu")
- [ ] Dropdown options are announced correctly
- [ ] Current theme state is communicated

#### Color Contrast (WCAG AA standards)
- [ ] Light mode text on light background: ≥4.5:1 ratio
- [ ] Dark mode text on dark background: ≥4.5:1 ratio
- [ ] Links are distinguishable from text
- [ ] Use WebAIM contrast checker

#### Zoom/Scale
- [ ] Page works at 200% zoom
- [ ] Toggle button accessible at high zoom
- [ ] Text remains readable
- [ ] No overlapping elements

### 5. Browser Compatibility

Test in these browsers:

- [ ] **Chrome** (latest)
- [ ] **Firefox** (latest)
- [ ] **Safari** (latest)
- [ ] **Edge** (latest)
- [ ] **Chrome Mobile** (latest)
- [ ] **Safari iOS** (latest)
- [ ] **Firefox Mobile** (latest)

### 6. System Preference Detection

#### macOS
- [ ] Enable system dark mode → blog auto-switches to dark
- [ ] Disable system dark mode → blog auto-switches to light
- [ ] System preference changes while page open → theme updates

#### Windows
- [ ] Enable dark mode in Settings → blog auto-switches to dark
- [ ] Disable dark mode in Settings → blog auto-switches to light
- [ ] Verify in browser DevTools: `prefers-color-scheme: dark`

#### Linux
- [ ] System preference respected (if using GNOME)
- [ ] Browser respects system theme setting

### 7. Content Testing

#### Blog Posts
- [ ] Post title readable in both modes
- [ ] Post content text readable
- [ ] Code snippets properly highlighted in both modes
- [ ] Links have proper contrast and are underlined
- [ ] Blockquotes have proper styling
- [ ] Lists (ordered and unordered) are readable

#### Images
- [ ] Images not too bright in dark mode
- [ ] Images have proper shadows/borders
- [ ] Transparent PNGs render correctly
- [ ] Images don't break layout

#### Tables
- [ ] Table headers visible in both modes
- [ ] Table borders clearly visible
- [ ] Alternating row colors work in dark mode
- [ ] Text in cells is readable

#### Forms (if present)
- [ ] Input fields visible in both modes
- [ ] Placeholders are visible
- [ ] Focus state is visible
- [ ] Labels are readable
- [ ] Error messages have proper contrast

### 8. Performance Testing

#### Load Time
- [ ] First Contentful Paint (FCP) doesn't increase
- [ ] Largest Contentful Paint (LCP) doesn't increase
- [ ] No layout shifts when theme loads

#### Memory
- [ ] No memory leaks with repeated toggling
- [ ] localStorage doesn't grow indefinitely
- [ ] Smooth toggling (no lag/stuttering)

#### CPU
- [ ] Theme toggle doesn't spike CPU usage
- [ ] Page animations remain smooth during toggle

### 9. Edge Cases

#### LocalStorage Issues
- [ ] Disabled localStorage → uses default theme
- [ ] Quota exceeded → graceful fallback
- [ ] Private/Incognito mode → works without persistence

#### System Preference Changes
- [ ] Change system theme while tab is in background → updates on focus
- [ ] System theme changes to media query preference → theme updates

#### Multiple Tabs
- [ ] Change theme in tab A → tab B updates (if using storage events)
- [ ] System theme change affects all open tabs

#### No JavaScript
- [ ] Page is readable without JavaScript
- [ ] Basic styling loads
- [ ] No console errors

### 10. Console Testing

Check browser DevTools console for:
- [ ] No hydration errors
- [ ] No "suppressHydrationWarning" related warnings
- [ ] No localStorage errors
- [ ] No React errors
- [ ] No network errors

## Manual Testing Checklist

```
DESKTOP
☐ Light mode loads correctly
☐ Dark mode toggle works
☐ Theme persists on reload
☐ System preference detected
☐ All text readable in both modes
☐ All images render properly
☐ No console errors

MOBILE
☐ Toggle button visible
☐ Hamburger menu works
☐ Theme toggle works on mobile
☐ Text readable at mobile size
☐ No horizontal scroll
☐ Touch targets proper size

ACCESSIBILITY
☐ Keyboard navigation works
☐ Tab order correct
☐ Focus visible
☐ ARIA labels present
☐ Color contrast acceptable
☐ Screen reader friendly

CROSS-BROWSER
☐ Chrome
☐ Firefox
☐ Safari
☐ Edge
```

## Automated Testing

```typescript
// Example Jest test
describe('Theme Toggle', () => {
  it('should toggle between light and dark modes', () => {
    // Test toggle functionality
  })

  it('should persist theme to localStorage', () => {
    // Test localStorage persistence
  })

  it('should respect system preference', () => {
    // Test system preference detection
  })

  it('should have proper ARIA labels', () => {
    // Test accessibility
  })
})
```

## Performance Benchmarks

- Theme toggle latency: < 100ms
- localStorage read: < 5ms
- CSS transition duration: 200-300ms
- First paint time: no increase
- Memory usage: < 1MB additional

## Known Issues to Watch For

1. **Hydration Mismatch**: If you see hydration errors, ensure `suppressHydrationWarning` is on `<html>`
2. **Flash of Unstyled Content (FOUC)**: Ensure theme loads before first paint
3. **localStorage Not Available**: Handle private/incognito mode gracefully
4. **System Preference Detection**: Not supported in all browsers (< Safari 13)

## Testing Tools

### Browser DevTools
- DevTools > Application > Local Storage (verify storage)
- DevTools > Console (check for errors)
- DevTools > Network (verify assets load)
- DevTools > Rendering (check paint timing)

### Browser Extensions
- [Lighthouse](https://developers.google.com/web/tools/lighthouse) (performance)
- [axe DevTools](https://www.deque.com/axe/devtools/) (accessibility)
- [WebAIM Contrast Checker](https://webaim.org/resources/contrastchecker/) (contrast)

### Online Tools
- [WAVE](https://wave.webaim.org/) (accessibility)
- [PageSpeed Insights](https://pagespeed.web.dev/) (performance)
- [Responsively App](https://responsively.app/) (responsive design)

## Test Results Template

```markdown
## Dark Mode Testing Results

**Date**: YYYY-MM-DD
**Tester**: [Name]
**Browser**: [Browser] [Version]
**OS**: [OS] [Version]

### Results
- Light Mode: ✅ Pass / ❌ Fail
- Dark Mode: ✅ Pass / ❌ Fail
- Theme Persistence: ✅ Pass / ❌ Fail
- System Preference: ✅ Pass / ❌ Fail
- Accessibility: ✅ Pass / ❌ Fail
- Mobile Responsive: ✅ Pass / ❌ Fail

### Issues Found
1. [Issue Description]
2. [Issue Description]

### Notes
[Any additional observations]
```

## Sign-Off Checklist

- [ ] All critical tests passed
- [ ] No P1 (critical) issues remaining
- [ ] No P2 (major) issues remaining
- [ ] All browsers tested
- [ ] Mobile tested
- [ ] Accessibility compliant
- [ ] Performance acceptable
- [ ] Ready for production

## Contact

For issues or questions about testing, contact the development team.
