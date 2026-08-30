(function () {
  'use strict';

  var storageKey = 'forgectl.docs.reader.v1';
  var defaults = {
    bodyFont: 'literary',
    headingFont: 'humanist',
    codeFont: 'jetbrains',
    fontSize: '18',
    lineHeight: '1.72',
    measure: '72'
  };
  var families = {
    bodyFont: {
      literary: '"Iowan Old Style", "Palatino Linotype", Charter, Georgia, serif',
      humanist: '"iA Writer Quattro", "Avenir Next", "Source Sans 3", system-ui, sans-serif',
      system: '-apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif',
      mono: '"JetBrains Mono", "Berkeley Mono", ui-monospace, SFMono-Regular, Menlo, monospace'
    },
    headingFont: {
      humanist: '"Avenir Next", Avenir, "Source Sans 3", system-ui, sans-serif',
      literary: '"Iowan Old Style", "Palatino Linotype", Charter, Georgia, serif',
      system: '-apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif',
      mono: '"JetBrains Mono", "Berkeley Mono", ui-monospace, SFMono-Regular, Menlo, monospace'
    },
    codeFont: {
      jetbrains: '"JetBrains Mono", ui-monospace, SFMono-Regular, Menlo, monospace',
      berkeley: '"Berkeley Mono", "JetBrains Mono", ui-monospace, SFMono-Regular, Menlo, monospace',
      system: 'ui-monospace, SFMono-Regular, Menlo, Monaco, Consolas, monospace'
    }
  };
  var properties = {
    bodyFont: '--reader-body-font',
    headingFont: '--reader-heading-font',
    codeFont: '--reader-code-font',
    fontSize: '--reader-font-size',
    lineHeight: '--reader-line-height',
    measure: '--reader-measure'
  };

  function load() {
    try {
      var saved = JSON.parse(localStorage.getItem(storageKey) || '{}');
      return Object.assign({}, defaults, saved);
    } catch (_) {
      return Object.assign({}, defaults);
    }
  }

  function save(settings) {
    try {
      localStorage.setItem(storageKey, JSON.stringify(settings));
    } catch (_) {
      // A locked-down browser may deny storage. The current page still works.
    }
  }

  function cssValue(name, value) {
    if (families[name]) return families[name][value] || families[name][defaults[name]];
    if (name === 'fontSize') return value + 'px';
    if (name === 'measure') return value + 'ch';
    return value;
  }

  function renderValue(control) {
    var output = document.querySelector('[data-reader-value="' + control.dataset.readerSetting + '"]');
    if (!output) return;
    var suffix = control.dataset.readerSetting === 'fontSize' ? 'px' :
      control.dataset.readerSetting === 'measure' ? 'ch' : '';
    output.textContent = control.value + suffix;
  }

  function apply(settings) {
    Object.keys(properties).forEach(function (name) {
      document.documentElement.style.setProperty(properties[name], cssValue(name, settings[name]));
      var control = document.querySelector('[data-reader-setting="' + name + '"]');
      if (control) {
        control.value = settings[name];
        renderValue(control);
      }
    });
  }

  var settings = load();
  apply(settings);

  document.querySelectorAll('[data-reader-setting]').forEach(function (control) {
    control.addEventListener('input', function () {
      settings[control.dataset.readerSetting] = control.value;
      apply(settings);
      save(settings);
    });
  });

  var reset = document.querySelector('[data-reader-reset]');
  if (reset) {
    reset.addEventListener('click', function () {
      settings = Object.assign({}, defaults);
      apply(settings);
      save(settings);
    });
  }
})();
