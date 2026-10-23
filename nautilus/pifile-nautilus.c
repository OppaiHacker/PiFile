/* Nautilus (Files) extension for PiFile.
 *
 * - "Convert…" in the right-click menu of files and folders.
 * - Middle click on a file opens the PiFile radial menu for it (or for the
 *   whole selection when the clicked file is selected).
 *
 * All conversion logic lives in the Go `pifile` binary; this only hands it paths.
 *
 * Build:   cc -shared -fPIC -O2 -o libpifile-nautilus.so pifile-nautilus.c \
 *            $(pkg-config --cflags --libs libnautilus-extension-4 gtk4)
 * Install: sudo install -m644 libpifile-nautilus.so /usr/lib/nautilus/extensions-4/
 */
#include <nautilus-extension.h>
#include <gtk/gtk.h>

static const char *
pifile_bin (void)
{
  static char *path;
  if (path == NULL)
    {
      path = g_find_program_in_path ("pifile");
      if (path == NULL) /* Nautilus is often started without ~/.local/bin in PATH */
        path = g_build_filename (g_get_home_dir (), ".local", "bin", "pifile", NULL);
    }
  return path;
}

/* files: list of NautilusFileInfo */
static void
run_pick (GList *files)
{
  GPtrArray *argv = g_ptr_array_new_with_free_func (g_free);
  g_ptr_array_add (argv, g_strdup (pifile_bin ()));
  g_ptr_array_add (argv, g_strdup ("pick"));
  for (GList *l = files; l != NULL; l = l->next)
    {
      GFile *f = nautilus_file_info_get_location (NAUTILUS_FILE_INFO (l->data));
      char *path = g_file_get_path (f);
      g_object_unref (f);
      if (path != NULL)
        g_ptr_array_add (argv, path);
    }
  if (argv->len > 2)
    {
      GError *err = NULL;
      g_ptr_array_add (argv, NULL);
      if (!g_spawn_async (NULL, (char **) argv->pdata, NULL, G_SPAWN_DEFAULT, NULL, NULL, NULL, &err))
        {
          g_warning ("pifile: %s", err->message);
          g_error_free (err);
        }
    }
  g_ptr_array_unref (argv);
}

/* ---- right-click menu ---- */

static void
on_activate (NautilusMenuItem *item, gpointer data)
{
  run_pick (g_object_get_data (G_OBJECT (item), "pifile-files"));
}

static GList *
get_file_items (NautilusMenuProvider *provider, GList *files)
{
  if (files == NULL)
    return NULL;
  for (GList *l = files; l != NULL; l = l->next)
    {
      GFile *f = nautilus_file_info_get_location (NAUTILUS_FILE_INFO (l->data));
      gboolean native = g_file_is_native (f);
      g_object_unref (f);
      if (!native)
        return NULL;
    }
  NautilusMenuItem *item = nautilus_menu_item_new ("PiFile::convert", "Convert…",
                                                   "Convert, join, pack or extract with PiFile",
                                                   "document-export");
  g_object_set_data_full (G_OBJECT (item), "pifile-files", nautilus_file_info_list_copy (files),
                          (GDestroyNotify) nautilus_file_info_list_free);
  g_signal_connect (item, "activate", G_CALLBACK (on_activate), NULL);
  return g_list_append (NULL, item);
}

static void
menu_iface_init (NautilusMenuProviderInterface *iface)
{
  iface->get_file_items = get_file_items;
}

typedef struct { GObject parent; } PifileExt;
typedef struct { GObjectClass parent; } PifileExtClass;
static void pifile_ext_class_init (PifileExtClass *klass) {}
static void pifile_ext_init (PifileExt *self) {}
G_DEFINE_TYPE_WITH_CODE (PifileExt, pifile_ext, G_TYPE_OBJECT,
                         G_IMPLEMENT_INTERFACE (NAUTILUS_TYPE_MENU_PROVIDER, menu_iface_init))

/* ---- middle click ----
 * Nautilus has no public API for this, so we find the view cell under the
 * pointer and read its "item" -> "file" properties (stable across 43..51). */

static gboolean
has_prop (gpointer obj, const char *name)
{
  return g_object_class_find_property (G_OBJECT_GET_CLASS (obj), name) != NULL;
}

/* Returns a new reference to the NautilusFileInfo behind a view item, or NULL. */
static NautilusFileInfo *
item_file (GObject *item)
{
  GObject *it = item ? g_object_ref (item) : NULL;
  if (it != NULL && GTK_IS_TREE_LIST_ROW (it))
    {
      GObject *inner = gtk_tree_list_row_get_item (GTK_TREE_LIST_ROW (it));
      g_object_unref (it);
      it = inner;
    }
  NautilusFileInfo *file = NULL;
  if (it != NULL && has_prop (it, "file"))
    {
      GObject *f = NULL;
      g_object_get (it, "file", &f, NULL);
      if (f != NULL && NAUTILUS_IS_FILE_INFO (f))
        file = NAUTILUS_FILE_INFO (f);
      else if (f != NULL)
        g_object_unref (f);
    }
  if (it != NULL)
    g_object_unref (it);
  return file;
}

static GtkSelectionModel *
view_model (GtkWidget *w)
{
  if (GTK_IS_GRID_VIEW (w))
    return gtk_grid_view_get_model (GTK_GRID_VIEW (w));
  if (GTK_IS_LIST_VIEW (w))
    return gtk_list_view_get_model (GTK_LIST_VIEW (w));
  if (GTK_IS_COLUMN_VIEW (w))
    return gtk_column_view_get_model (GTK_COLUMN_VIEW (w));
  return NULL;
}

static gboolean
same_file (NautilusFileInfo *a, NautilusFileInfo *b)
{
  char *ua = nautilus_file_info_get_uri (a), *ub = nautilus_file_info_get_uri (b);
  gboolean eq = g_strcmp0 (ua, ub) == 0;
  g_free (ua);
  g_free (ub);
  return eq;
}

static void
on_middle_pressed (GtkGestureClick *gesture, int n_press, double x, double y, GtkWidget *window)
{
  GtkWidget *picked = gtk_widget_pick (window, x, y, GTK_PICK_DEFAULT);
  NautilusFileInfo *clicked = NULL;
  GtkSelectionModel *model = NULL;
  for (GtkWidget *w = picked; w != NULL && w != window; w = gtk_widget_get_parent (w))
    {
      if (clicked == NULL && has_prop (w, "item"))
        {
          GObject *item = NULL;
          g_object_get (w, "item", &item, NULL);
          if (item != NULL)
            {
              clicked = item_file (item);
              g_object_unref (item);
            }
        }
      if (model == NULL)
        model = view_model (w);
    }
  if (clicked == NULL)
    return; /* not on a file: let Nautilus handle the click */

  gtk_gesture_set_state (GTK_GESTURE (gesture), GTK_EVENT_SEQUENCE_CLAIMED);

  GList *files = NULL;
  gboolean clicked_selected = FALSE;
  if (model != NULL)
    {
      GtkBitset *sel = gtk_selection_model_get_selection (model);
      GtkBitsetIter iter;
      guint pos;
      for (gboolean ok = gtk_bitset_iter_init_first (&iter, sel, &pos); ok; ok = gtk_bitset_iter_next (&iter, &pos))
        {
          GObject *item = g_list_model_get_item (G_LIST_MODEL (model), pos);
          NautilusFileInfo *f = item_file (item);
          if (item != NULL)
            g_object_unref (item);
          if (f == NULL)
            continue;
          if (same_file (f, clicked))
            clicked_selected = TRUE;
          files = g_list_append (files, f);
        }
      gtk_bitset_unref (sel);
    }
  if (!clicked_selected)
    {
      nautilus_file_info_list_free (files);
      files = g_list_append (NULL, g_object_ref (clicked));
    }
  run_pick (files);
  nautilus_file_info_list_free (files);
  g_object_unref (clicked);
}

static void
hook_window (GtkWidget *window)
{
  if (!GTK_IS_WINDOW (window) || g_object_get_data (G_OBJECT (window), "pifile-hooked"))
    return;
  g_object_set_data (G_OBJECT (window), "pifile-hooked", GINT_TO_POINTER (1));
  GtkGesture *g = gtk_gesture_click_new ();
  gtk_gesture_single_set_button (GTK_GESTURE_SINGLE (g), GDK_BUTTON_MIDDLE);
  gtk_event_controller_set_propagation_phase (GTK_EVENT_CONTROLLER (g), GTK_PHASE_CAPTURE);
  g_signal_connect (g, "pressed", G_CALLBACK (on_middle_pressed), window);
  gtk_widget_add_controller (window, GTK_EVENT_CONTROLLER (g));
}

static void
on_toplevels_changed (GListModel *list, guint pos, guint removed, guint added, gpointer data)
{
  for (guint i = pos; i < pos + added; i++)
    {
      GObject *w = g_list_model_get_item (list, i);
      hook_window (GTK_WIDGET (w));
      g_object_unref (w);
    }
}

static gboolean
hook_all (gpointer data)
{
  static gboolean done;
  if (done)
    return G_SOURCE_REMOVE;
  done = TRUE;
  GListModel *tops = gtk_window_get_toplevels ();
  on_toplevels_changed (tops, 0, 0, g_list_model_get_n_items (tops), NULL);
  g_signal_connect (tops, "items-changed", G_CALLBACK (on_toplevels_changed), NULL);
  return G_SOURCE_REMOVE;
}

/* Lets the middle-click part be tried without installing: LD_PRELOAD + PIFILE_PRELOAD=1. */
__attribute__ ((constructor)) static void
preload (void)
{
  if (g_getenv ("PIFILE_PRELOAD") != NULL)
    g_idle_add (hook_all, NULL);
}

/* ---- module entry points ---- */

static GType types[1];

void
nautilus_module_initialize (GTypeModule *module)
{
  types[0] = pifile_ext_get_type ();
  g_idle_add (hook_all, NULL); /* GTK may not be ready while modules load */
}

void
nautilus_module_shutdown (void)
{
}

void
nautilus_module_list_types (const GType **out, int *n)
{
  *out = types;
  *n = G_N_ELEMENTS (types);
}
