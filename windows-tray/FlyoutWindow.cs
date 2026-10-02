using System.Diagnostics;
using System.Globalization;
using System.Net;
using System.Text.Json;
using System.Windows;
using System.Windows.Automation;
using System.Windows.Controls;
using System.Windows.Controls.Primitives;
using System.Windows.Data;
using System.Windows.Input;
using System.Windows.Media;
using System.Windows.Threading;

namespace DoubleTake.Tray;

internal sealed class FlyoutWindow : Window
{
    private readonly CancellationTokenSource lifetime = new();
    private readonly string preferencePath = Path.Combine(BackendSession.StateDirectory, "receiver.json");
    private readonly TextBlock receiverTitle = Text("Add a receiver", 16, false);
    private readonly TextBlock receiverEndpoint = Text("Choose a nearby receiver or enter its IP", 12, true);
    private readonly TextBlock connectionStatus = Text("Ready to connect", 12, true);
    private readonly TextBlock stateLabel = Text("Not connected", 12, true);
    private readonly TextBlock discoveryStatus = Text("", 12, true);
    private readonly Button connectionButton = ActionButton("Connect", "ConnectionAction", "Connect to receiver", true);
    private readonly Button refreshButton = IconButton("\uE72C", "RefreshReceivers", "Refresh receivers");
    private readonly Button settingsButton = IconButton("\uE713", "ReceiverSettings", "Receiver settings");
    private readonly TextBox address = Input("ReceiverAddress", "Receiver IP address");
    private readonly TextBox port = Input("ReceiverPort", "AirPlay port");
    private readonly TextBox receiverName = Input("ReceiverName", "Receiver name");
    private readonly ComboBox receivers = new() { MinHeight = 32, DisplayMemberPath = "Name", Margin = new Thickness(0, 0, 0, 8) };
    private readonly CheckBox pairAgain = new() { Content = "Pair again on next connection", Margin = new Thickness(0, 8, 0, 8) };
    private readonly StackPanel advanced = new() { Visibility = Visibility.Collapsed, Margin = new Thickness(0, 12, 0, 0) };
    private readonly StackPanel pairing = new() { Visibility = Visibility.Collapsed, Margin = new Thickness(0, 12, 0, 0) };
    private readonly TextBlock pairingPrompt = Text("Enter the PIN shown on your receiver", 12, true);
    private readonly PasswordBox credential = new() { MinHeight = 36, Padding = new Thickness(8), Margin = new Thickness(0, 8, 0, 8) };
    private readonly Button continueButton = ActionButton("Continue", "ContinuePairing", "Continue pairing", true);
    private readonly Button cancelButton = ActionButton("Cancel", "CancelPairing", "Cancel pairing and disconnect");
    private BackendSession? session;
    private Receiver? selectedReceiver;
    private Receiver? activeReceiver;
    private BackendEvent? credentialRequest;
    private Task? sessionTask;
    private Task? discoveryTask;
    private bool discovering;
    private bool shuttingDown;
    private bool sessionReady;
    private bool hadError;
    private bool submittingCredential;
    private bool editingInternally;
    private bool initialized;
    private int targetRevision;
    private string nameAddress = "";

    private sealed record Preference(string IP, int Port, string? Name = null);

    internal event Action? RevealRequested;
    internal event Action<string, bool>? StatusChanged;
    internal event Action? QuitRequested;
    internal bool IsConnected => sessionReady;
    internal bool IsInteractionPinned => credentialRequest != null || submittingCredential ||
        (session != null && !sessionReady && !session.StopRequested) ||
        (advanced.IsVisible && (address.IsKeyboardFocusWithin || port.IsKeyboardFocusWithin || receiverName.IsKeyboardFocusWithin));

    internal FlyoutWindow()
    {
        Title = "DoubleTake";
        Width = 368;
        SizeToContent = SizeToContent.Height;
        MaxHeight = Math.Max(320, SystemParameters.WorkArea.Height - 24);
        WindowStyle = WindowStyle.None;
        ResizeMode = ResizeMode.NoResize;
        ShowInTaskbar = false;
        AllowsTransparency = false;
        FontFamily = new FontFamily("Segoe UI");
        FontSize = 14;
        UseLayoutRounding = true;
        SnapsToDevicePixels = true;
        SetResourceReference(BackgroundProperty, "Surface");
        SetResourceReference(ForegroundProperty, "Text");
        AutomationProperties.SetName(this, "DoubleTake receiver flyout");
        AutomationProperties.SetAutomationId(this, "DoubleTakeFlyout");
        DefineStyles();
        ApplyTheme(FlyoutTheme.GetPalette());
        Content = BuildSurface();
        AutomationProperties.SetName(receivers, "Nearby AirPlay receivers");
        AutomationProperties.SetAutomationId(receivers, "NearbyReceivers");
        AutomationProperties.SetName(pairAgain, "Pair again on next connection");
        AutomationProperties.SetAutomationId(pairAgain, "PairAgain");
        AutomationProperties.SetName(credential, "Receiver pairing PIN or password");
        AutomationProperties.SetAutomationId(credential, "ReceiverPIN");
        AutomationProperties.SetAutomationId(connectionStatus, "ConnectionStatus");
        AutomationProperties.SetLiveSetting(connectionStatus, AutomationLiveSetting.Polite);
        AutomationProperties.SetAutomationId(receiverTitle, "ReceiverIdentity");
        AutomationProperties.SetAutomationId(stateLabel, "ConnectionState");
        connectionButton.Click += async (_, _) =>
        {
            if (session == null) await ConnectAsync();
            else await DisconnectAsync();
        };
        refreshButton.Click += async (_, _) => await DiscoverAsync();
        settingsButton.Click += (_, _) =>
        {
            advanced.Visibility = advanced.IsVisible ? Visibility.Collapsed : Visibility.Visible;
            settingsButton.ToolTip = advanced.IsVisible ? "Hide receiver settings" : "Receiver settings";
            if (advanced.IsVisible) address.Focus();
        };
        receivers.SelectionChanged += (_, _) =>
        {
            if (!editingInternally && session == null && receivers.SelectedItem is Receiver receiver) SelectReceiver(receiver);
        };
        address.TextChanged += (_, _) =>
        {
            if (editingInternally) return;
            targetRevision++;
            if (!SameAddress(address.Text, nameAddress))
            {
                editingInternally = true;
                receiverName.Clear();
                editingInternally = false;
                nameAddress = address.Text.Trim();
            }
            UpdateIdentity();
        };
        port.TextChanged += (_, _) => { if (!editingInternally) { targetRevision++; UpdateIdentity(); } };
        receiverName.TextChanged += (_, _) =>
        {
            if (!editingInternally) { targetRevision++; nameAddress = address.Text.Trim(); UpdateIdentity(); }
        };
        credential.PasswordChanged += (_, _) => UpdateControls();
        continueButton.Click += async (_, _) => await SubmitCredentialAsync();
        cancelButton.Click += async (_, _) => await DisconnectAsync();
        credential.PreviewKeyDown += async (_, e) =>
        {
            if (e.Key != Key.Escape) return;
            e.Handled = true;
            await DisconnectAsync();
        };
        Loaded += async (_, _) =>
        {
            if (initialized) return;
            initialized = true;
            await DiscoverAsync();
        };
        LoadPreference();
        UpdateIdentity();
        UpdateControls();
    }

    private UIElement BuildSurface()
    {
        var body = new StackPanel { Margin = new Thickness(16) };
        var identity = new Grid();
        identity.ColumnDefinitions.Add(new ColumnDefinition { Width = new GridLength(36) });
        identity.ColumnDefinitions.Add(new ColumnDefinition());
        var receiverGlyph = Glyph("\uE7F4", 24);
        receiverGlyph.VerticalAlignment = VerticalAlignment.Center;
        identity.Children.Add(receiverGlyph);
        var identityText = new StackPanel();
        receiverTitle.FontWeight = FontWeights.SemiBold;
        receiverTitle.TextTrimming = TextTrimming.CharacterEllipsis;
        receiverEndpoint.Margin = new Thickness(0, 4, 0, 0);
        identityText.Children.Add(receiverTitle);
        identityText.Children.Add(receiverEndpoint);
        Grid.SetColumn(identityText, 1);
        identity.Children.Add(identityText);
        body.Children.Add(identity);
        var statusRow = new StackPanel { Margin = new Thickness(0, 16, 0, 12) };
        stateLabel.FontWeight = FontWeights.SemiBold;
        connectionStatus.Margin = new Thickness(0, 4, 0, 0);
        statusRow.Children.Add(stateLabel);
        statusRow.Children.Add(connectionStatus);
        body.Children.Add(statusRow);
        var sharing = new StackPanel { Margin = new Thickness(12, 8, 12, 8) };
        sharing.Children.Add(SummaryRow("\uE7F4", "Screen", "Windows desktop"));
        sharing.Children.Add(SummaryRow("\uE767", "Audio", "System output"));
        var sharingCard = new Border { CornerRadius = new CornerRadius(8), BorderThickness = new Thickness(1), Child = sharing, Margin = new Thickness(0, 0, 0, 12) };
        sharingCard.SetResourceReference(Border.BackgroundProperty, "Card");
        sharingCard.SetResourceReference(Border.BorderBrushProperty, "Border");
        body.Children.Add(sharingCard);
        connectionButton.HorizontalAlignment = HorizontalAlignment.Stretch;
        body.Children.Add(connectionButton);
        pairing.Children.Add(pairingPrompt);
        credential.SetResourceReference(Control.BackgroundProperty, "Card");
        credential.SetResourceReference(Control.ForegroundProperty, "Text");
        credential.SetResourceReference(Control.BorderBrushProperty, "Border");
        pairing.Children.Add(credential);
        var pinActions = new Grid();
        pinActions.ColumnDefinitions.Add(new ColumnDefinition());
        pinActions.ColumnDefinitions.Add(new ColumnDefinition());
        cancelButton.Margin = new Thickness(0, 0, 4, 0);
        continueButton.Margin = new Thickness(4, 0, 0, 0);
        pinActions.Children.Add(cancelButton);
        Grid.SetColumn(continueButton, 1);
        pinActions.Children.Add(continueButton);
        pairing.Children.Add(pinActions);
        body.Children.Add(pairing);
        advanced.Children.Add(Separator());
        advanced.Children.Add(Text("Receiver settings", 14, false));
        advanced.Children.Add(discoveryStatus);
        discoveryStatus.Margin = new Thickness(0, 4, 0, 8);
        advanced.Children.Add(receivers);
        advanced.Children.Add(Field("Name", receiverName));
        advanced.Children.Add(Field("IP address", address));
        advanced.Children.Add(Field("AirPlay port", port));
        port.Text = "7000";
        advanced.Children.Add(pairAgain);
        advanced.Children.Add(Text("Audio captures the default Windows output. No extra audio device is needed.", 12, true));
        var helpActions = new Grid { Margin = new Thickness(0, 12, 0, 0) };
        helpActions.ColumnDefinitions.Add(new ColumnDefinition());
        helpActions.ColumnDefinitions.Add(new ColumnDefinition());
        var logs = ActionButton("Open log", "OpenLog", "Open local diagnostic log");
        var quit = ActionButton("Quit", "QuitDoubleTake", "Quit DoubleTake and stop sharing");
        logs.Margin = new Thickness(0, 0, 4, 0);
        quit.Margin = new Thickness(4, 0, 0, 0);
        logs.Click += (_, _) => OpenLog();
        quit.Click += (_, _) => QuitRequested?.Invoke();
        helpActions.Children.Add(logs);
        Grid.SetColumn(quit, 1);
        helpActions.Children.Add(quit);
        advanced.Children.Add(helpActions);
        body.Children.Add(advanced);
        body.Children.Add(Separator());
        var footer = new Grid();
        footer.ColumnDefinitions.Add(new ColumnDefinition());
        footer.ColumnDefinitions.Add(new ColumnDefinition { Width = GridLength.Auto });
        footer.ColumnDefinitions.Add(new ColumnDefinition { Width = GridLength.Auto });
        var product = Text("DoubleTake", 12, true);
        product.VerticalAlignment = VerticalAlignment.Center;
        footer.Children.Add(product);
        Grid.SetColumn(refreshButton, 1);
        Grid.SetColumn(settingsButton, 2);
        footer.Children.Add(refreshButton);
        footer.Children.Add(settingsButton);
        body.Children.Add(footer);
        var scroll = new ScrollViewer { Content = body, VerticalScrollBarVisibility = ScrollBarVisibility.Auto, HorizontalScrollBarVisibility = ScrollBarVisibility.Disabled, Focusable = false };
        var surface = new Border { CornerRadius = new CornerRadius(12), BorderThickness = new Thickness(1), Child = scroll };
        surface.SetResourceReference(Border.BorderBrushProperty, "Border");
        surface.SetResourceReference(Border.BackgroundProperty, "Surface");
        return surface;
    }

    internal void ApplyTheme(FlyoutPalette palette)
    {
        static SolidColorBrush Brush(Color color) { var brush = new SolidColorBrush(color); brush.Freeze(); return brush; }
        Color surface = palette.Surface;
        surface.A = palette.TransparencyEnabled && !palette.HighContrast ? (byte)235 : (byte)255;
        Resources["Surface"] = Brush(surface);
        Resources["Card"] = Brush(palette.Card);
        Resources["Border"] = Brush(palette.Border);
        Resources["Text"] = Brush(palette.Text);
        Resources["SecondaryText"] = Brush(palette.SecondaryText);
        Resources["DisabledText"] = Brush(palette.HighContrast ? SystemColors.GrayTextColor : palette.SecondaryText);
        Resources["Accent"] = Brush(palette.Accent);
        Resources["AccentText"] = Brush(palette.AccentText);
        Resources["Error"] = Brush(palette.HighContrast ? palette.Text : palette.IsDark ? Color.FromRgb(255, 170, 164) : Color.FromRgb(173, 35, 35));
        Resources[SystemColors.WindowBrushKey] = Resources["Card"];
        Resources[SystemColors.WindowTextBrushKey] = Resources["Text"];
        Resources[SystemColors.ControlBrushKey] = Resources["Card"];
        Resources[SystemColors.ControlTextBrushKey] = Resources["Text"];
        Resources[SystemColors.HighlightBrushKey] = Resources["Accent"];
        Resources[SystemColors.HighlightTextBrushKey] = Resources["AccentText"];
    }

    private void DefineStyles()
    {
        var buttonStyle = new Style(typeof(Button));
        buttonStyle.Setters.Add(new Setter(Control.BackgroundProperty, new DynamicResourceExtension("Card")));
        buttonStyle.Setters.Add(new Setter(Control.ForegroundProperty, new DynamicResourceExtension("Text")));
        buttonStyle.Setters.Add(new Setter(Control.BorderBrushProperty, new DynamicResourceExtension("Border")));
        buttonStyle.Setters.Add(new Setter(Control.BorderThicknessProperty, new Thickness(1)));
        buttonStyle.Setters.Add(new Setter(Control.PaddingProperty, new Thickness(12, 8, 12, 8)));
        buttonStyle.Setters.Add(new Setter(FrameworkElement.MinHeightProperty, 36.0));
        buttonStyle.Setters.Add(new Setter(Control.CursorProperty, Cursors.Hand));
        var border = new FrameworkElementFactory(typeof(Border));
        border.Name = "ButtonBorder";
        border.SetValue(Border.CornerRadiusProperty, new CornerRadius(6));
        border.SetValue(Border.BackgroundProperty, new TemplateBindingExtension(Control.BackgroundProperty));
        border.SetValue(Border.BorderBrushProperty, new TemplateBindingExtension(Control.BorderBrushProperty));
        border.SetValue(Border.BorderThicknessProperty, new TemplateBindingExtension(Control.BorderThicknessProperty));
        border.SetValue(Border.PaddingProperty, new TemplateBindingExtension(Control.PaddingProperty));
        var content = new FrameworkElementFactory(typeof(ContentPresenter));
        content.SetValue(ContentPresenter.HorizontalAlignmentProperty, HorizontalAlignment.Center);
        content.SetValue(ContentPresenter.VerticalAlignmentProperty, VerticalAlignment.Center);
        border.AppendChild(content);
        var template = new ControlTemplate(typeof(Button)) { VisualTree = border };
        var hover = new Trigger { Property = UIElement.IsMouseOverProperty, Value = true };
        hover.Setters.Add(new Setter(UIElement.OpacityProperty, 0.85, "ButtonBorder"));
        template.Triggers.Add(hover);
        var pressed = new Trigger { Property = Button.IsPressedProperty, Value = true };
        pressed.Setters.Add(new Setter(UIElement.OpacityProperty, 0.75, "ButtonBorder"));
        template.Triggers.Add(pressed);
        var focus = new Trigger { Property = UIElement.IsKeyboardFocusedProperty, Value = true };
        focus.Setters.Add(new Setter(Border.BorderBrushProperty, new DynamicResourceExtension("Text"), "ButtonBorder"));
        focus.Setters.Add(new Setter(Border.BorderThicknessProperty, new Thickness(2), "ButtonBorder"));
        template.Triggers.Add(focus);
        buttonStyle.Setters.Add(new Setter(Control.TemplateProperty, template));
        var disabled = new Trigger { Property = UIElement.IsEnabledProperty, Value = false };
        disabled.Setters.Add(new Setter(UIElement.OpacityProperty, 0.45));
        disabled.Setters.Add(new Setter(Control.CursorProperty, Cursors.Arrow));
        buttonStyle.Triggers.Add(disabled);
        Resources[typeof(Button)] = buttonStyle;
        var inputStyle = new Style(typeof(TextBox));
        inputStyle.Setters.Add(new Setter(Control.BackgroundProperty, new DynamicResourceExtension("Card")));
        inputStyle.Setters.Add(new Setter(Control.ForegroundProperty, new DynamicResourceExtension("Text")));
        inputStyle.Setters.Add(new Setter(Control.BorderBrushProperty, new DynamicResourceExtension("Border")));
        inputStyle.Setters.Add(new Setter(Control.PaddingProperty, new Thickness(8, 6, 8, 6)));
        var inputFocus = new Trigger { Property = UIElement.IsKeyboardFocusedProperty, Value = true };
        inputFocus.Setters.Add(new Setter(Control.BorderBrushProperty, new DynamicResourceExtension("Accent")));
        inputStyle.Triggers.Add(inputFocus);
        Resources[typeof(TextBox)] = inputStyle;
        var comboStyle = new Style(typeof(ComboBox));
        comboStyle.Setters.Add(new Setter(Control.BackgroundProperty, new DynamicResourceExtension("Card")));
        comboStyle.Setters.Add(new Setter(Control.ForegroundProperty, new DynamicResourceExtension("Text")));
        comboStyle.Setters.Add(new Setter(Control.BorderBrushProperty, new DynamicResourceExtension("Border")));
        comboStyle.Setters.Add(new Setter(Control.BorderThicknessProperty, new Thickness(1)));
        comboStyle.Setters.Add(new Setter(Control.PaddingProperty, new Thickness(10, 6, 32, 6)));
        comboStyle.Setters.Add(new Setter(Control.FocusVisualStyleProperty, null));
        var comboBorder = new FrameworkElementFactory(typeof(Border), "ReceiverBorder");
        comboBorder.SetValue(Border.CornerRadiusProperty, new CornerRadius(6));
        comboBorder.SetValue(Border.BackgroundProperty, new TemplateBindingExtension(Control.BackgroundProperty));
        comboBorder.SetValue(Border.BorderBrushProperty, new TemplateBindingExtension(Control.BorderBrushProperty));
        comboBorder.SetValue(Border.BorderThicknessProperty, new TemplateBindingExtension(Control.BorderThicknessProperty));
        var comboGrid = new FrameworkElementFactory(typeof(Grid));
        var toggle = new FrameworkElementFactory(typeof(ToggleButton));
        toggle.SetValue(UIElement.FocusableProperty, false);
        toggle.SetValue(KeyboardNavigation.IsTabStopProperty, false);
        toggle.SetValue(Control.ForegroundProperty, new TemplateBindingExtension(Control.ForegroundProperty));
        toggle.SetValue(AutomationProperties.NameProperty, "Show nearby receivers");
        toggle.SetBinding(ToggleButton.IsCheckedProperty, new Binding(nameof(ComboBox.IsDropDownOpen))
        {
            RelativeSource = new RelativeSource(RelativeSourceMode.TemplatedParent), Mode = BindingMode.TwoWay
        });
        var toggleSurface = new FrameworkElementFactory(typeof(Border));
        toggleSurface.SetValue(Border.BackgroundProperty, Brushes.Transparent);
        var arrow = new FrameworkElementFactory(typeof(TextBlock));
        arrow.SetValue(TextBlock.TextProperty, "\uE70D");
        arrow.SetValue(TextBlock.FontFamilyProperty, new FontFamily("Segoe Fluent Icons, Segoe MDL2 Assets"));
        arrow.SetValue(TextBlock.FontSizeProperty, 12.0);
        arrow.SetValue(TextBlock.ForegroundProperty, new TemplateBindingExtension(Control.ForegroundProperty));
        arrow.SetValue(FrameworkElement.HorizontalAlignmentProperty, HorizontalAlignment.Right);
        arrow.SetValue(FrameworkElement.VerticalAlignmentProperty, VerticalAlignment.Center);
        arrow.SetValue(FrameworkElement.MarginProperty, new Thickness(0, 0, 10, 0));
        toggleSurface.AppendChild(arrow);
        toggle.SetValue(Control.TemplateProperty, new ControlTemplate(typeof(ToggleButton)) { VisualTree = toggleSurface });
        comboGrid.AppendChild(toggle);
        var selection = new FrameworkElementFactory(typeof(ContentPresenter));
        selection.SetValue(ContentPresenter.ContentProperty, new TemplateBindingExtension(ComboBox.SelectionBoxItemProperty));
        selection.SetValue(ContentPresenter.ContentTemplateProperty, new TemplateBindingExtension(ComboBox.SelectionBoxItemTemplateProperty));
        selection.SetValue(UIElement.ClipToBoundsProperty, true);
        selection.SetValue(ContentPresenter.ContentStringFormatProperty, new TemplateBindingExtension(ComboBox.SelectionBoxItemStringFormatProperty));
        selection.SetValue(FrameworkElement.MarginProperty, new TemplateBindingExtension(Control.PaddingProperty));
        selection.SetValue(FrameworkElement.VerticalAlignmentProperty, VerticalAlignment.Center);
        selection.SetValue(UIElement.IsHitTestVisibleProperty, false);
        comboGrid.AppendChild(selection);
        var popup = new FrameworkElementFactory(typeof(Popup), "PART_Popup");
        popup.SetValue(Popup.PlacementProperty, PlacementMode.Bottom);
        popup.SetValue(Popup.AllowsTransparencyProperty, true);
        popup.SetValue(UIElement.FocusableProperty, false);
        popup.SetBinding(Popup.IsOpenProperty, new Binding(nameof(ComboBox.IsDropDownOpen))
        {
            RelativeSource = new RelativeSource(RelativeSourceMode.TemplatedParent), Mode = BindingMode.TwoWay
        });
        popup.SetBinding(Popup.PlacementTargetProperty, new Binding
        {
            RelativeSource = new RelativeSource(RelativeSourceMode.TemplatedParent)
        });
        var listBorder = new FrameworkElementFactory(typeof(Border));
        listBorder.SetResourceReference(Border.BackgroundProperty, "Card");
        listBorder.SetResourceReference(Border.BorderBrushProperty, "Border");
        listBorder.SetValue(Border.BorderThicknessProperty, new Thickness(1));
        listBorder.SetValue(Border.CornerRadiusProperty, new CornerRadius(6));
        listBorder.SetValue(Border.PaddingProperty, new Thickness(4));
        listBorder.SetBinding(FrameworkElement.MinWidthProperty, new Binding(nameof(ActualWidth))
        {
            RelativeSource = new RelativeSource(RelativeSourceMode.TemplatedParent)
        });
        listBorder.SetValue(KeyboardNavigation.TabNavigationProperty, KeyboardNavigationMode.Cycle);
        var listScroll = new FrameworkElementFactory(typeof(ScrollViewer));
        listScroll.SetValue(FrameworkElement.MaxHeightProperty, new TemplateBindingExtension(ComboBox.MaxDropDownHeightProperty));
        listScroll.SetValue(ScrollViewer.CanContentScrollProperty, true);
        listScroll.SetValue(ScrollViewer.VerticalScrollBarVisibilityProperty, ScrollBarVisibility.Auto);
        listScroll.SetValue(ScrollViewer.HorizontalScrollBarVisibilityProperty, ScrollBarVisibility.Disabled);
        listScroll.SetValue(UIElement.FocusableProperty, false);
        var items = new FrameworkElementFactory(typeof(ItemsPresenter));
        items.SetValue(KeyboardNavigation.DirectionalNavigationProperty, KeyboardNavigationMode.Contained);
        listScroll.AppendChild(items);
        listBorder.AppendChild(listScroll);
        popup.AppendChild(listBorder);
        comboGrid.AppendChild(popup);
        comboBorder.AppendChild(comboGrid);
        var comboTemplate = new ControlTemplate(typeof(ComboBox)) { VisualTree = comboBorder };
        var comboHover = new Trigger { Property = UIElement.IsMouseOverProperty, Value = true };
        comboHover.Setters.Add(new Setter(Border.BorderBrushProperty, new DynamicResourceExtension("SecondaryText"), "ReceiverBorder"));
        comboTemplate.Triggers.Add(comboHover);
        var comboFocus = new Trigger { Property = UIElement.IsKeyboardFocusWithinProperty, Value = true };
        comboFocus.Setters.Add(new Setter(Border.BorderBrushProperty, new DynamicResourceExtension("Text"), "ReceiverBorder"));
        comboFocus.Setters.Add(new Setter(Border.BorderThicknessProperty, new Thickness(2), "ReceiverBorder"));
        comboTemplate.Triggers.Add(comboFocus);
        var comboOpen = new Trigger { Property = ComboBox.IsDropDownOpenProperty, Value = true };
        comboOpen.Setters.Add(new Setter(Border.BorderBrushProperty, new DynamicResourceExtension("Text"), "ReceiverBorder"));
        comboTemplate.Triggers.Add(comboOpen);
        comboStyle.Setters.Add(new Setter(Control.TemplateProperty, comboTemplate));
        var comboDisabled = new Trigger { Property = UIElement.IsEnabledProperty, Value = false };
        comboDisabled.Setters.Add(new Setter(Control.ForegroundProperty, new DynamicResourceExtension("DisabledText")));
        comboDisabled.Setters.Add(new Setter(Control.BorderBrushProperty, new DynamicResourceExtension("DisabledText")));
        comboStyle.Triggers.Add(comboDisabled);
        var noReceivers = new Trigger { Property = ItemsControl.HasItemsProperty, Value = false };
        noReceivers.Setters.Add(new Setter(UIElement.VisibilityProperty, Visibility.Collapsed));
        comboStyle.Triggers.Add(noReceivers);
        Resources[typeof(ComboBox)] = comboStyle;
        var itemStyle = new Style(typeof(ComboBoxItem));
        itemStyle.Setters.Add(new Setter(Control.ForegroundProperty, new DynamicResourceExtension("Text")));
        itemStyle.Setters.Add(new Setter(Control.BackgroundProperty, new DynamicResourceExtension("Card")));
        itemStyle.Setters.Add(new Setter(Control.PaddingProperty, new Thickness(8, 6, 8, 6)));
        itemStyle.Setters.Add(new Setter(Control.FocusVisualStyleProperty, null));
        var itemBorder = new FrameworkElementFactory(typeof(Border), "ReceiverItemBorder");
        itemBorder.SetValue(Border.CornerRadiusProperty, new CornerRadius(3));
        itemBorder.SetValue(Border.BackgroundProperty, new TemplateBindingExtension(Control.BackgroundProperty));
        itemBorder.SetValue(Border.BorderThicknessProperty, new Thickness(1));
        itemBorder.SetValue(Border.PaddingProperty, new TemplateBindingExtension(Control.PaddingProperty));
        var itemContent = new FrameworkElementFactory(typeof(ContentPresenter));
        itemContent.SetValue(ContentPresenter.VerticalAlignmentProperty, VerticalAlignment.Center);
        itemBorder.AppendChild(itemContent);
        var itemTemplate = new ControlTemplate(typeof(ComboBoxItem)) { VisualTree = itemBorder };
        foreach (var property in new[] { ComboBoxItem.IsHighlightedProperty, ComboBoxItem.IsSelectedProperty })
        {
            var selected = new Trigger { Property = property, Value = true };
            selected.Setters.Add(new Setter(Border.BackgroundProperty, new DynamicResourceExtension("Accent"), "ReceiverItemBorder"));
            selected.Setters.Add(new Setter(Control.ForegroundProperty, new DynamicResourceExtension("AccentText")));
            itemTemplate.Triggers.Add(selected);
        }
        var itemFocus = new Trigger { Property = UIElement.IsKeyboardFocusedProperty, Value = true };
        itemFocus.Setters.Add(new Setter(Border.BorderBrushProperty, new DynamicResourceExtension("AccentText"), "ReceiverItemBorder"));
        itemTemplate.Triggers.Add(itemFocus);
        itemStyle.Setters.Add(new Setter(Control.TemplateProperty, itemTemplate));
        Resources[typeof(ComboBoxItem)] = itemStyle;

        var checkStyle = new Style(typeof(CheckBox));
        checkStyle.Setters.Add(new Setter(Control.ForegroundProperty, new DynamicResourceExtension("Text")));
        checkStyle.Setters.Add(new Setter(Control.FocusVisualStyleProperty, null));
        checkStyle.Setters.Add(new Setter(FrameworkElement.MinHeightProperty, 28.0));
        var checkFocusBorder = new FrameworkElementFactory(typeof(Border), "CheckFocusBorder");
        checkFocusBorder.SetValue(Border.CornerRadiusProperty, new CornerRadius(4));
        checkFocusBorder.SetValue(Border.BorderThicknessProperty, new Thickness(1));
        checkFocusBorder.SetValue(Border.PaddingProperty, new Thickness(2));
        checkFocusBorder.SetValue(Border.BackgroundProperty, Brushes.Transparent);
        var checkLayout = new FrameworkElementFactory(typeof(DockPanel));
        var checkBox = new FrameworkElementFactory(typeof(Border), "CheckBoxBorder");
        checkBox.SetValue(FrameworkElement.WidthProperty, 18.0);
        checkBox.SetValue(FrameworkElement.HeightProperty, 18.0);
        checkBox.SetValue(FrameworkElement.VerticalAlignmentProperty, VerticalAlignment.Center);
        checkBox.SetValue(Border.CornerRadiusProperty, new CornerRadius(3));
        checkBox.SetValue(Border.BorderThicknessProperty, new Thickness(1));
        checkBox.SetResourceReference(Border.BackgroundProperty, "Card");
        checkBox.SetResourceReference(Border.BorderBrushProperty, "SecondaryText");
        var checkMark = new FrameworkElementFactory(typeof(TextBlock), "CheckMark");
        checkMark.SetValue(TextBlock.TextProperty, "\uE73E");
        checkMark.SetValue(TextBlock.FontFamilyProperty, new FontFamily("Segoe Fluent Icons, Segoe MDL2 Assets"));
        checkMark.SetValue(TextBlock.FontSizeProperty, 12.0);
        checkMark.SetResourceReference(TextBlock.ForegroundProperty, "AccentText");
        checkMark.SetValue(FrameworkElement.HorizontalAlignmentProperty, HorizontalAlignment.Center);
        checkMark.SetValue(FrameworkElement.VerticalAlignmentProperty, VerticalAlignment.Center);
        checkMark.SetValue(UIElement.VisibilityProperty, Visibility.Collapsed);
        checkBox.AppendChild(checkMark);
        checkLayout.AppendChild(checkBox);
        var checkContent = new FrameworkElementFactory(typeof(ContentPresenter));
        checkContent.SetValue(FrameworkElement.MarginProperty, new Thickness(8, 0, 0, 0));
        checkContent.SetValue(FrameworkElement.VerticalAlignmentProperty, VerticalAlignment.Center);
        checkContent.SetValue(ContentPresenter.RecognizesAccessKeyProperty, true);
        checkLayout.AppendChild(checkContent);
        checkFocusBorder.AppendChild(checkLayout);
        var checkTemplate = new ControlTemplate(typeof(CheckBox)) { VisualTree = checkFocusBorder };
        var checkHover = new Trigger { Property = UIElement.IsMouseOverProperty, Value = true };
        checkHover.Setters.Add(new Setter(Border.BorderBrushProperty, new DynamicResourceExtension("Text"), "CheckBoxBorder"));
        checkTemplate.Triggers.Add(checkHover);
        var checkedTrigger = new Trigger { Property = ToggleButton.IsCheckedProperty, Value = true };
        checkedTrigger.Setters.Add(new Setter(Border.BackgroundProperty, new DynamicResourceExtension("Accent"), "CheckBoxBorder"));
        checkedTrigger.Setters.Add(new Setter(Border.BorderBrushProperty, new DynamicResourceExtension("Accent"), "CheckBoxBorder"));
        checkedTrigger.Setters.Add(new Setter(UIElement.VisibilityProperty, Visibility.Visible, "CheckMark"));
        checkTemplate.Triggers.Add(checkedTrigger);
        var checkFocus = new Trigger { Property = UIElement.IsKeyboardFocusedProperty, Value = true };
        checkFocus.Setters.Add(new Setter(Border.BorderBrushProperty, new DynamicResourceExtension("Text"), "CheckFocusBorder"));
        checkTemplate.Triggers.Add(checkFocus);
        var checkDisabled = new Trigger { Property = UIElement.IsEnabledProperty, Value = false };
        checkDisabled.Setters.Add(new Setter(Control.ForegroundProperty, new DynamicResourceExtension("DisabledText")));
        checkDisabled.Setters.Add(new Setter(Border.BackgroundProperty, new DynamicResourceExtension("Card"), "CheckBoxBorder"));
        checkDisabled.Setters.Add(new Setter(Border.BorderBrushProperty, new DynamicResourceExtension("DisabledText"), "CheckBoxBorder"));
        checkDisabled.Setters.Add(new Setter(TextBlock.ForegroundProperty, new DynamicResourceExtension("DisabledText"), "CheckMark"));
        checkTemplate.Triggers.Add(checkDisabled);
        checkStyle.Setters.Add(new Setter(Control.TemplateProperty, checkTemplate));
        Resources[typeof(CheckBox)] = checkStyle;
    }

    private static TextBlock Text(string text, double size, bool secondary)
    {
        var block = new TextBlock { Text = text, FontSize = size, TextWrapping = TextWrapping.Wrap };
        block.SetResourceReference(TextBlock.ForegroundProperty, secondary ? "SecondaryText" : "Text");
        return block;
    }

    private static TextBlock Glyph(string glyph, double size) => new() { Text = glyph, FontFamily = new FontFamily("Segoe Fluent Icons, Segoe MDL2 Assets"), FontSize = size };

    private static Button ActionButton(string text, string id, string name, bool primary = false)
    {
        var button = new Button { Content = text };
        AutomationProperties.SetAutomationId(button, id);
        AutomationProperties.SetName(button, name);
        if (primary)
        {
            button.SetResourceReference(Control.BackgroundProperty, "Accent");
            button.SetResourceReference(Control.ForegroundProperty, "AccentText");
        }
        return button;
    }

    private static Button IconButton(string glyph, string id, string name)
    {
        var button = ActionButton("", id, name);
        button.Content = Glyph(glyph, 16);
        button.ToolTip = name;
        button.Padding = new Thickness(8);
        button.MinWidth = 36;
        button.SetResourceReference(Control.BackgroundProperty, "Surface");
        button.BorderThickness = new Thickness(0);
        return button;
    }

    private static TextBox Input(string id, string name)
    {
        var input = new TextBox { MinHeight = 32 };
        AutomationProperties.SetAutomationId(input, id);
        AutomationProperties.SetName(input, name);
        return input;
    }

    private static FrameworkElement Field(string name, Control control)
    {
        var stack = new StackPanel { Margin = new Thickness(0, 0, 0, 8) };
        var label = new Label { Content = name, Target = control, Padding = new Thickness(0, 0, 0, 4), FontSize = 12 };
        label.SetResourceReference(Control.ForegroundProperty, "SecondaryText");
        stack.Children.Add(label);
        stack.Children.Add(control);
        return stack;
    }

    private static FrameworkElement SummaryRow(string glyph, string label, string value)
    {
        var row = new Grid { Margin = new Thickness(0, 4, 0, 4) };
        row.ColumnDefinitions.Add(new ColumnDefinition { Width = new GridLength(28) });
        row.ColumnDefinitions.Add(new ColumnDefinition());
        row.ColumnDefinitions.Add(new ColumnDefinition { Width = GridLength.Auto });
        row.Children.Add(Glyph(glyph, 16));
        var title = Text(label, 14, false);
        Grid.SetColumn(title, 1);
        row.Children.Add(title);
        var summary = Text(value, 12, true);
        summary.VerticalAlignment = VerticalAlignment.Center;
        Grid.SetColumn(summary, 2);
        row.Children.Add(summary);
        return row;
    }

    private static Border Separator()
    {
        var separator = new Border { Height = 1, Margin = new Thickness(0, 12, 0, 12) };
        separator.SetResourceReference(Border.BackgroundProperty, "Border");
        return separator;
    }

    private static bool SameAddress(string left, string right) =>
        IPAddress.TryParse(left.Trim(), out var a) && IPAddress.TryParse(right.Trim(), out var b) && a.Equals(b);

    private void SelectReceiver(Receiver receiver)
    {
        selectedReceiver = receiver;
        editingInternally = true;
        address.Text = receiver.IP;
        port.Text = receiver.Port.ToString(CultureInfo.InvariantCulture);
        receiverName.Text = receiver.Name;
        nameAddress = receiver.IP;
        editingInternally = false;
        targetRevision++;
        UpdateIdentity();
    }

    private void UpdateIdentity()
    {
        string ip = activeReceiver?.IP ?? address.Text.Trim();
        string name = activeReceiver?.Name ?? receiverName.Text.Trim();
        receiverTitle.Text = name.Length > 0 ? name : ip.Length > 0 ? "AirPlay receiver" : "Add a receiver";
        receiverEndpoint.Text = ip.Length > 0 ? $"{ip} · {activeReceiver?.Port.ToString(CultureInfo.InvariantCulture) ?? port.Text.Trim()}" : "Choose a nearby receiver or enter its IP";
    }

    private void UpdateControls()
    {
        bool idle = session == null && !shuttingDown;
        address.IsEnabled = port.IsEnabled = receiverName.IsEnabled = receivers.IsEnabled = pairAgain.IsEnabled = idle;
        refreshButton.IsEnabled = idle && !discovering;
        connectionButton.Content = session != null ? session.StopRequested ? "Disconnecting…" : "Disconnect" : "Connect";
        connectionButton.IsEnabled = !shuttingDown && (session == null || !session.StopRequested);
        AutomationProperties.SetName(connectionButton, session == null ? "Connect to receiver" : "Disconnect from receiver");
        continueButton.IsEnabled = credentialRequest != null && credential.Password.Length > 0 && !submittingCredential;
        continueButton.IsDefault = pairing.IsVisible;
        cancelButton.IsEnabled = session != null && !session.StopRequested;
        stateLabel.Text = hadError ? "Connection error" : sessionReady ? "Connected" : credentialRequest != null ? "Pairing required" : session != null ? session.StopRequested ? "Disconnecting" : "Connecting" : "Not connected";
        stateLabel.SetResourceReference(TextBlock.ForegroundProperty, hadError ? "Error" : "Text");
    }

    private void SetStatus(string message)
    {
        connectionStatus.Text = message;
        connectionStatus.SetResourceReference(TextBlock.ForegroundProperty, hadError ? "Error" : "SecondaryText");
        UpdateControls();
        StatusChanged?.Invoke(message, sessionReady);
    }

    private void ShowError(string message, bool sessionError = false)
    {
        hadError = true;
        if (sessionError) sessionReady = false;
        SetStatus(message);
        RevealRequested?.Invoke();
    }

    private Task DiscoverAsync()
    {
        if (discovering || session != null || shuttingDown) return Task.CompletedTask;
        discoveryTask = DiscoverCoreAsync();
        return discoveryTask;
    }

    private async Task DiscoverCoreAsync()
    {
        discovering = true;
        int revision = targetRevision;
        discoveryStatus.Text = "Finding nearby AirPlay receivers…";
        if (address.Text.Trim().Length == 0 && !hadError) SetStatus("Finding nearby receivers…");
        UpdateControls();
        try
        {
            var found = await BackendSession.DiscoverAsync(lifetime.Token);
            if (shuttingDown) return;
            editingInternally = true;
            receivers.ItemsSource = found;
            receivers.SelectedItem = found.FirstOrDefault(item => SameAddress(item.IP, address.Text));
            editingInternally = false;
            if (session == null && targetRevision == revision)
            {
                if (address.Text.Trim().Length == 0 && found.Length > 0) SelectReceiver(found[0]);
                else if (receivers.SelectedItem is Receiver matching)
                {
                    selectedReceiver = matching;
                    if (receiverName.Text.Trim().Length == 0)
                    {
                        editingInternally = true;
                        receiverName.Text = matching.Name;
                        nameAddress = address.Text.Trim();
                        editingInternally = false;
                        UpdateIdentity();
                    }
                }
            }
            discoveryStatus.Text = found.Length > 0 ? "Select a nearby receiver, or use a saved IP." : "No nearby receivers found. A saved or manual IP still works, including with a VPN.";
            if (session == null && !hadError) SetStatus(address.Text.Trim().Length > 0 ? "Ready to connect" : "No receiver yet. Add its IP in receiver settings.");
        }
        catch (OperationCanceledException) when (lifetime.IsCancellationRequested) { }
        catch (Exception error)
        {
            discoveryStatus.Text = "Discovery unavailable. You can still connect using a saved or manual IP.";
            if (session == null && !shuttingDown && !hadError && address.Text.Trim().Length == 0) ShowError(error.Message);
        }
        finally { discovering = false; editingInternally = false; UpdateControls(); }
    }

    internal Task ConnectAsync()
    {
        if (session != null || shuttingDown) return Task.CompletedTask;
        if (!IPAddress.TryParse(address.Text.Trim(), out var ip))
        {
            advanced.Visibility = Visibility.Visible;
            ShowError("Enter a valid IPv4 or IPv6 receiver address.");
            address.Focus();
            return Task.CompletedTask;
        }
        if (!int.TryParse(port.Text.Trim(), NumberStyles.None, CultureInfo.InvariantCulture, out int targetPort) || targetPort is < 1 or > 65535)
        {
            advanced.Visibility = Visibility.Visible;
            ShowError("Enter an AirPlay port from 1 to 65535.");
            port.Focus();
            return Task.CompletedTask;
        }
        string name = receiverName.Text.Trim();
        var receiver = new Receiver
        {
            IP = ip.ToString(), Port = targetPort, Name = name.Length > 0 ? name : "AirPlay receiver",
            Advertisement = selectedReceiver != null && SameAddress(selectedReceiver.IP, ip.ToString()) && selectedReceiver.Port == targetPort ? selectedReceiver.Advertisement : null
        };
        BackendSession active;
        try { active = new BackendSession(receiver, pairAgain.IsChecked == true); }
        catch (Exception error) { ShowError(error.Message); return Task.CompletedTask; }
        session = active;
        activeReceiver = receiver;
        hadError = false;
        sessionReady = false;
        advanced.Visibility = Visibility.Collapsed;
        UpdateIdentity();
        SetStatus("Starting desktop and audio sharing…");
        active.EventReceived += item =>
        {
            if (!Dispatcher.HasShutdownStarted) Dispatcher.BeginInvoke(DispatcherPriority.Normal, new Action(() => OnBackendEvent(active, item)));
        };
        sessionTask = RunSessionAsync(active);
        return sessionTask;
    }

    private async Task RunSessionAsync(BackendSession active)
    {
        try
        {
            int exitCode = await active.RunAsync();
            if (!active.StopRequested && !hadError && !shuttingDown)
                ShowError(exitCode == 0 ? "The receiver ended the sharing session." : $"Sharing stopped (code {exitCode}). Open the local log for details.", true);
        }
        catch (Exception error) { if (!active.StopRequested && !shuttingDown) ShowError(error.Message, true); }
        finally
        {
            ClearCredential();
            if (ReferenceEquals(session, active)) session = null;
            sessionReady = false;
            activeReceiver = null;
            active.Dispose();
            UpdateIdentity();
            UpdateControls();
            if (!hadError) SetStatus("Desktop and audio sharing stopped.");
            else StatusChanged?.Invoke(connectionStatus.Text, false);
        }
    }

    private void OnBackendEvent(BackendSession active, BackendEvent item)
    {
        if (!ReferenceEquals(session, active) || shuttingDown) return;
        switch (item.Type)
        {
            case "credential_required":
                if (active.StopRequested || hadError) return;
                sessionReady = false;
                credentialRequest = item;
                submittingCredential = false;
                credential.Clear();
                pairingPrompt.Text = string.IsNullOrWhiteSpace(item.Message) ? "Enter the PIN shown on your receiver" : item.Message.Trim();
                pairing.Visibility = Visibility.Visible;
                SetStatus("Enter the receiver PIN to continue.");
                RevealRequested?.Invoke();
                Dispatcher.BeginInvoke(DispatcherPriority.Input, new Action(() => credential.Focus()));
                break;
            case "connected":
                if (!item.SessionReady || active.StopRequested || hadError) return;
                ClearCredential();
                sessionReady = true;
                pairAgain.IsChecked = false;
                SetStatus("Sharing your desktop and system audio");
                if (activeReceiver != null) SavePreference(activeReceiver);
                break;
            case "error":
                if (!active.StopRequested) { ClearCredential(); ShowError(item.Message, true); }
                break;
            case "warning":
                if (!hadError && !active.StopRequested) SetStatus(item.Message);
                break;
            case "disconnected":
                sessionReady = false;
                ClearCredential();
                if (!hadError) SetStatus("Stopping desktop and audio sharing…");
                break;
        }
    }

    private async Task SubmitCredentialAsync()
    {
        var active = session;
        var request = credentialRequest;
        if (active == null || request == null || active.StopRequested || submittingCredential || credential.Password.Length == 0) return;
        string value = credential.Password;
        submittingCredential = true;
        UpdateControls();
        try
        {
            await active.SendCredentialAsync(request.RequestId, value);
            if (ReferenceEquals(session, active) && ReferenceEquals(credentialRequest, request) && !active.StopRequested)
            {
                ClearCredential();
                SetStatus("Pairing and starting desktop/audio capture…");
            }
        }
        catch (Exception error)
        {
            if (!active.StopRequested) ShowError(error.Message, true);
            await DisconnectAsync();
        }
        finally { value = ""; submittingCredential = false; UpdateControls(); }
    }

    private void ClearCredential()
    {
        credentialRequest = null;
        credential.Clear();
        pairing.Visibility = Visibility.Collapsed;
        continueButton.IsDefault = false;
    }

    internal async Task DisconnectAsync()
    {
        var active = session;
        if (active == null) return;
        ClearCredential();
        if (!active.StopRequested)
        {
            if (!hadError) SetStatus("Stopping desktop and audio sharing…");
            Task stop = active.StopAsync();
            UpdateControls();
            await stop;
        }
        if (sessionTask != null) await sessionTask;
    }

    internal async Task ShutdownAsync()
    {
        if (shuttingDown) { if (sessionTask != null) await sessionTask; return; }
        shuttingDown = true;
        lifetime.Cancel();
        ClearCredential();
        UpdateControls();
        await DisconnectAsync();
        if (discoveryTask != null) await discoveryTask;
        lifetime.Dispose();
    }

    internal void EmergencyStop() => session?.EmergencyStop();

    private void LoadPreference()
    {
        try
        {
            if (!File.Exists(preferencePath)) return;
            var saved = JsonSerializer.Deserialize<Preference>(File.ReadAllText(preferencePath));
            if (saved == null || !IPAddress.TryParse(saved.IP, out var ip) || saved.Port is < 1 or > 65535) return;
            editingInternally = true;
            nameAddress = ip.ToString();
            address.Text = nameAddress;
            port.Text = saved.Port.ToString(CultureInfo.InvariantCulture);
            receiverName.Text = saved.Name ?? "";
            editingInternally = false;
        }
        catch (Exception error) when (error is IOException or UnauthorizedAccessException or JsonException)
        {
            SetStatus("Saved receiver could not be read. Add its IP in receiver settings.");
        }
        finally { editingInternally = false; }
    }

    private void SavePreference(Receiver receiver)
    {
        try
        {
            Directory.CreateDirectory(BackendSession.StateDirectory);
            File.WriteAllText(preferencePath, JsonSerializer.Serialize(new Preference(receiver.IP, receiver.Port, receiverName.Text.Trim().Length > 0 ? receiver.Name : null)));
        }
        catch (Exception error) when (error is IOException or UnauthorizedAccessException)
        {
            SetStatus("Sharing, but this receiver could not be saved.");
        }
    }

    internal void OpenLog()
    {
        try
        {
            Directory.CreateDirectory(BackendSession.StateDirectory);
            if (!File.Exists(BackendSession.LogPath)) File.WriteAllText(BackendSession.LogPath, "DoubleTake local diagnostics. Credentials are not recorded.\r\n");
            Process.Start(new ProcessStartInfo("notepad.exe") { UseShellExecute = true, ArgumentList = { BackendSession.LogPath } });
        }
        catch (Exception error) { ShowError("Could not open the local log: " + error.Message); }
    }
}
